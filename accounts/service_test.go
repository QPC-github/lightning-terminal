package accounts

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lightninglabs/lndclient"
	invpkg "github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

var (
	testErr = errors.New("something terrible happened")

	testExpiration = time.Now().Add(24 * time.Hour)
	testTimeout    = time.Millisecond * 500
	testInterval   = time.Millisecond * 20

	testHash2 = lntypes.Hash{99, 88, 77}
)

type mockLnd struct {
	lndclient.LightningClient
	lndclient.RouterClient

	mainErrChan chan error

	invoiceReq chan lndclient.InvoiceSubscriptionRequest
	paymentReq chan lntypes.Hash

	callErr      error
	errChan      chan error
	invoiceChan  chan *lndclient.Invoice
	paymentChans map[lntypes.Hash]chan lndclient.PaymentStatus
}

func newMockLnd() *mockLnd {
	return &mockLnd{
		mainErrChan: make(chan error, 10),
		invoiceReq: make(
			chan lndclient.InvoiceSubscriptionRequest, 10,
		),
		paymentReq:  make(chan lntypes.Hash, 10),
		errChan:     make(chan error, 10),
		invoiceChan: make(chan *lndclient.Invoice),
		paymentChans: make(
			map[lntypes.Hash]chan lndclient.PaymentStatus,
		),
	}
}

func (m *mockLnd) assertNoMainErr(t *testing.T) {
	select {
	case err := <-m.mainErrChan:
		t.Fatalf("Expected no main err, got %v", err)

	default:
	}
}

func (m *mockLnd) assertMainErr(t *testing.T, expectedErr error) {
	select {
	case err := <-m.mainErrChan:
		require.Equal(t, expectedErr, err)

	case <-time.After(testTimeout):
		t.Fatalf("Did not get expected main err before timeout")
	}
}

func (m *mockLnd) assertNoInvoiceRequest(t *testing.T) {
	select {
	case req := <-m.invoiceReq:
		t.Fatalf("Expected no invoice request, got %v", req)

	default:
	}
}

func (m *mockLnd) assertInvoiceRequest(t *testing.T, addIndex,
	settleIndex uint64) {

	select {
	case invoiceReq := <-m.invoiceReq:
		require.Equal(t, addIndex, invoiceReq.AddIndex)
		require.Equal(t, settleIndex, invoiceReq.SettleIndex)

	case <-time.After(testTimeout):
		t.Fatalf("Did not get expected invoice request before timeout")
	}
}

func (m *mockLnd) assertNoPaymentRequest(t *testing.T) {
	select {
	case req := <-m.paymentReq:
		t.Fatalf("Expected no payment request, got %v", req)

	default:
	}
}

func (m *mockLnd) assertPaymentRequests(t *testing.T,
	hashes map[lntypes.Hash]struct{}) {

	overallTimeout := time.After(testTimeout)

	for {
		select {
		case hash := <-m.paymentReq:
			require.Contains(t, hashes, hash)
			delete(hashes, hash)

			// Did we get all expected hashes?
			if len(hashes) == 0 {
				return
			}

		case <-overallTimeout:
			t.Fatalf("Did not get %d expected hashes before "+
				"timeout", len(hashes))
		}
	}
}

// SubscribeInvoices allows a client to subscribe to updates of newly
// added/settled invoices.
func (m *mockLnd) SubscribeInvoices(_ context.Context,
	req lndclient.InvoiceSubscriptionRequest) (<-chan *lndclient.Invoice,
	<-chan error, error) {

	if m.callErr != nil {
		return nil, nil, m.callErr
	}

	m.invoiceReq <- req

	return m.invoiceChan, m.errChan, nil
}

// TrackPayment picks up a previously started payment and returns a payment
// update stream and an error stream.
func (m *mockLnd) TrackPayment(_ context.Context,
	hash lntypes.Hash) (chan lndclient.PaymentStatus, chan error, error) {

	if m.callErr != nil {
		return nil, nil, m.callErr
	}

	m.paymentReq <- hash
	m.paymentChans[hash] = make(chan lndclient.PaymentStatus, 1)

	return m.paymentChans[hash], m.errChan, nil
}

// TestAccountService tests that the account service can track payments and
// invoices of account related calls correctly.
func TestAccountService(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name  string
		setup func(t *testing.T, lnd *mockLnd,
			s *InterceptorService)
		startupErr string
		validate   func(t *testing.T, lnd *mockLnd,
			s *InterceptorService)
	}{{
		name: "startup err on tracking payment",
		setup: func(t *testing.T, lnd *mockLnd, s *InterceptorService) {
			lnd.callErr = testErr
		},
		startupErr: testErr.Error(),
		validate: func(t *testing.T, lnd *mockLnd,
			s *InterceptorService) {

			lnd.assertNoInvoiceRequest(t)
		},
	}, {
		name: "goroutine err sent on main err chan",
		setup: func(t *testing.T, lnd *mockLnd, s *InterceptorService) {
			acct := &OffChainBalanceAccount{
				ID:             testID,
				Type:           TypeInitialBalance,
				CurrentBalance: 1234,
				Invoices: map[lntypes.Hash]struct{}{
					testHash: {},
				},
				Payments: make(map[lntypes.Hash]*PaymentEntry),
			}

			err := s.store.UpdateAccount(acct)
			require.NoError(t, err)

			lnd.errChan <- testErr
		},
		validate: func(t *testing.T, lnd *mockLnd,
			s *InterceptorService) {

			lnd.assertInvoiceRequest(t, 0, 0)
			lnd.assertMainErr(t, testErr)
		},
	}, {
		name: "startup do not track completed payments",
		setup: func(t *testing.T, lnd *mockLnd, s *InterceptorService) {
			acct, err := s.store.NewAccount(1234, testExpiration)
			require.NoError(t, err)

			acct.Invoices[testHash] = struct{}{}
			acct.Payments[testHash] = &PaymentEntry{
				Status:     lnrpc.Payment_FAILED,
				FullAmount: 1234,
			}

			err = s.store.UpdateAccount(acct)
			require.NoError(t, err)
		},
		validate: func(t *testing.T, lnd *mockLnd,
			s *InterceptorService) {

			require.Contains(t, s.invoiceToAccount, testHash)
			lnd.assertNoPaymentRequest(t)
			lnd.assertInvoiceRequest(t, 0, 0)
			lnd.assertNoMainErr(t)
		},
	}, {
		name: "startup track in-flight payments",
		setup: func(t *testing.T, lnd *mockLnd, s *InterceptorService) {
			acct := &OffChainBalanceAccount{
				ID:             testID,
				Type:           TypeInitialBalance,
				CurrentBalance: 1234,
				Invoices: map[lntypes.Hash]struct{}{
					testHash: {},
				},
				Payments: map[lntypes.Hash]*PaymentEntry{
					testHash: {
						Status:     lnrpc.Payment_IN_FLIGHT,
						FullAmount: 1234,
					},
					testHash2: {
						Status:     lnrpc.Payment_UNKNOWN,
						FullAmount: 3456,
					},
				},
			}

			err := s.store.UpdateAccount(acct)
			require.NoError(t, err)
		},
		validate: func(t *testing.T, lnd *mockLnd,
			s *InterceptorService) {

			require.Contains(t, s.invoiceToAccount, testHash)
			lnd.assertPaymentRequests(t, map[lntypes.Hash]struct{}{
				testHash:  {},
				testHash2: {},
			})
			lnd.assertInvoiceRequest(t, 0, 0)
			lnd.assertNoMainErr(t)

			// Send an actual payment update and make sure the
			// amount is debited from the account.
			lnd.paymentChans[testHash] <- lndclient.PaymentStatus{
				State: lnrpc.Payment_SUCCEEDED,
				Fee:   234,
				Value: 1000,
			}

			assertEventually(t, func() bool {
				acct, err := s.store.Account(testID)
				require.NoError(t, err)

				return acct.CurrentBalance == 0
			})

			// Remove the other payment and make sure it disappears
			// from the tracked payments and is also updated
			// correctly in the account store.
			lnd.paymentChans[testHash2] <- lndclient.PaymentStatus{
				State: lnrpc.Payment_FAILED,
				Fee:   234,
				Value: 1000,
			}

			assertEventually(t, func() bool {
				acct, err := s.store.Account(testID)
				require.NoError(t, err)

				if len(acct.Payments) != 2 {
					return false
				}

				p, ok := acct.Payments[testHash2]
				if !ok {
					return false
				}

				return p.Status == lnrpc.Payment_FAILED
			})

			require.NotContains(t, s.pendingPayments, testHash2)
		},
	}, {
		name: "keep track of invoice indexes",
		setup: func(t *testing.T, lnd *mockLnd, s *InterceptorService) {
			err := s.store.StoreLastIndexes(987_654, 555_555)
			require.NoError(t, err)
		},
		validate: func(t *testing.T, lnd *mockLnd,
			s *InterceptorService) {

			// We expect the initial subscription to start at the
			// indexes we stored in the DB.
			lnd.assertInvoiceRequest(t, 987_654, 555_555)

			// If we now send a new invoice, we expect the store to
			// track the latest index (but only those that are
			// bigger).
			lnd.invoiceChan <- &lndclient.Invoice{
				AddIndex:    123,
				SettleIndex: 666_666,
			}

			assertEventually(t, func() bool {
				addIdx, settleIdx, err := s.store.LastIndexes()
				require.NoError(t, err)

				if addIdx != 987_654 {
					return false
				}

				return settleIdx == 666_666
			})

			// Update again, this time only the add index.
			lnd.invoiceChan <- &lndclient.Invoice{
				AddIndex:    1_000_000,
				SettleIndex: 666_666,
			}

			assertEventually(t, func() bool {
				addIdx, settleIdx, err := s.store.LastIndexes()
				require.NoError(t, err)

				if addIdx != 1_000_000 {
					return false
				}

				return settleIdx == 666_666
			})
		},
	}, {
		name: "credit account",
		setup: func(t *testing.T, lnd *mockLnd, s *InterceptorService) {
			acct := &OffChainBalanceAccount{
				ID:             testID,
				Type:           TypeInitialBalance,
				CurrentBalance: 1234,
				Invoices: map[lntypes.Hash]struct{}{
					testHash: {},
				},
				Payments: make(map[lntypes.Hash]*PaymentEntry),
			}

			err := s.store.UpdateAccount(acct)
			require.NoError(t, err)
		},
		validate: func(t *testing.T, lnd *mockLnd,
			s *InterceptorService) {

			lnd.assertInvoiceRequest(t, 0, 0)
			lnd.invoiceChan <- &lndclient.Invoice{
				AddIndex:    12,
				SettleIndex: 12,
				Hash:        testHash,
				AmountPaid:  777,
				State:       invpkg.ContractSettled,
			}

			// Make sure the amount paid is eventually credited.
			assertEventually(t, func() bool {
				acct, err := s.store.Account(testID)
				require.NoError(t, err)

				return acct.CurrentBalance == (1234 + 777)
			})
		},
	}, {
		name: "in-flight payments",
		setup: func(t *testing.T, lnd *mockLnd, s *InterceptorService) {
			// We set up our account with a balance of 5k msats and
			// two in-flight payments with a total or 3k msats.
			acct := &OffChainBalanceAccount{
				ID:             testID,
				Type:           TypeInitialBalance,
				CurrentBalance: 5000,
				Invoices: map[lntypes.Hash]struct{}{
					testHash: {},
				},
				Payments: map[lntypes.Hash]*PaymentEntry{
					testHash: {
						Status:     lnrpc.Payment_IN_FLIGHT,
						FullAmount: 2000,
					},
					testHash2: {
						Status:     lnrpc.Payment_IN_FLIGHT,
						FullAmount: 1000,
					},
				},
			}

			err := s.store.UpdateAccount(acct)
			require.NoError(t, err)
		},
		validate: func(t *testing.T, lnd *mockLnd,
			s *InterceptorService) {

			// We should be able to initiate another payment with an
			// amount smaller or equal to 2k msats.
			err := s.CheckBalance(testID, 2000)
			require.NoError(t, err)

			// But exactly one sat over it should fail.
			err = s.CheckBalance(testID, 2001)
			require.ErrorIs(t, err, ErrAccBalanceInsufficient)

			// Remove one of the payments (to simulate it failed)
			// and try again.
			lnd.paymentChans[testHash] <- lndclient.PaymentStatus{
				State: lnrpc.Payment_FAILED,
			}

			// We should now have up to 4k msats available.
			assertEventually(t, func() bool {
				err = s.CheckBalance(testID, 4000)
				return err == nil
			})
		},
	}}

	for _, tc := range testCases {
		tc := tc

		t.Run(tc.name, func(tt *testing.T) {
			tt.Parallel()

			lndMock := newMockLnd()
			service, err := NewService(
				t.TempDir(), lndMock.mainErrChan,
			)
			require.NoError(t, err)

			// Is a setup call required to initialize initial
			// conditions?
			if tc.setup != nil {
				tc.setup(t, lndMock, service)
			}

			// Any errors during startup expected?
			err = service.Start(lndMock, lndMock, chainParams)
			if tc.startupErr != "" {
				require.ErrorContains(tt, err, tc.startupErr)

				lndMock.assertNoMainErr(t)

				if tc.validate != nil {
					tc.validate(tt, lndMock, service)
				}

				return
			}

			// Any post execution validation that we need to run?
			if tc.validate != nil {
				tc.validate(tt, lndMock, service)
			}

			err = service.Stop()
			require.NoError(tt, err)
			lndMock.assertNoMainErr(t)
		})
	}
}

// assertEventually asserts that the given predicate is eventually satisfied.
func assertEventually(t *testing.T, predicate func() bool) {
	require.Eventually(t, predicate, testTimeout, testInterval)
}
