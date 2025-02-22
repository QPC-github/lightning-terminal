package firewalldb

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math/big"
	"strconv"
	"strings"

	"github.com/lightninglabs/lightning-terminal/session"
	"go.etcd.io/bbolt"
)

/*
	The PrivacyMapper data is stored in the following structure in the db:

	privacy -> session id -> real-to-pseudo -> {k:v}
			      -> pseudo-to-real -> {k:v}
*/

const (
	txidStringLen = 64
)

var (
	privacyBucketKey = []byte("privacy")
	realToPseudoKey  = []byte("real-to-pseudo")
	pseudoToRealKey  = []byte("pseudo-to-real")

	pseudoStrAlphabet    = []rune("abcdef0123456789")
	pseudoStrAlphabetLen = len(pseudoStrAlphabet)
)

// NewPrivacyMapDB is a function type that takes a session ID and uses it to
// construct a new PrivacyMapDB.
type NewPrivacyMapDB func(sessionID session.ID) PrivacyMapDB

// PrivacyDB constructs a PrivacyMapDB that will be indexed under the given
// sessionID key.
func (db *DB) PrivacyDB(sessionID session.ID) PrivacyMapDB {
	return &privacyMapDB{
		DB:        db,
		sessionID: sessionID,
	}
}

// PrivacyMapDB provides an Update and View method that will allow the caller
// to perform atomic read and write transactions defined by PrivacyMapTx on the
// underlying DB.
type PrivacyMapDB interface {
	// Update opens a database read/write transaction and executes the
	// function f with the transaction passed as a parameter. After f exits,
	// if f did not error, the transaction is committed. Otherwise, if f did
	// error, the transaction is rolled back. If the rollback fails, the
	// original error returned by f is still returned. If the commit fails,
	// the commit error is returned.
	Update(f func(tx PrivacyMapTx) error) error

	// View opens a database read transaction and executes the function f
	// with the transaction passed as a parameter. After f exits, the
	// transaction is rolled back. If f errors, its error is returned, not a
	// rollback error (if any occur).
	View(f func(tx PrivacyMapTx) error) error
}

// PrivacyMapTx represents a db that can be used to create, store and fetch
// real-pseudo pairs.
type PrivacyMapTx interface {
	// NewPair persists a new real-pseudo pair.
	NewPair(real, pseudo string) error

	// PseudoToReal returns the real value associated with the given pseudo
	// value. If no such pair is found, then ErrNoSuchKeyFound is returned.
	PseudoToReal(pseudo string) (string, error)

	// RealToPseudo returns the pseudo value associated with the given real
	// value. If no such pair is found, then ErrNoSuchKeyFound is returned.
	RealToPseudo(real string) (string, error)
}

// privacyMapDB is an implementation of PrivacyMapDB.
type privacyMapDB struct {
	*DB
	sessionID session.ID
}

// beginTx starts db transaction. The transaction will be a read or read-write
// transaction depending on the value of the `writable` parameter.
func (p *privacyMapDB) beginTx(writable bool) (*privacyMapTx, error) {
	boltTx, err := p.Begin(writable)
	if err != nil {
		return nil, err
	}
	return &privacyMapTx{
		privacyMapDB: p,
		boltTx:       boltTx,
	}, nil
}

// Update opens a database read/write transaction and executes the function f
// with the transaction passed as a parameter. After f exits, if f did not
// error, the transaction is committed. Otherwise, if f did error, the
// transaction is rolled back. If the rollback fails, the original error
// returned by f is still returned. If the commit fails, the commit error is
// returned.
//
// NOTE: this is part of the PrivacyMapDB interface.
func (p *privacyMapDB) Update(f func(tx PrivacyMapTx) error) error {
	tx, err := p.beginTx(true)
	if err != nil {
		return err
	}

	// Make sure the transaction rolls back in the event of a panic.
	defer func() {
		if tx != nil {
			_ = tx.boltTx.Rollback()
		}
	}()

	err = f(tx)
	if err != nil {
		// Want to return the original error, not a rollback error if
		// any occur.
		_ = tx.boltTx.Rollback()
		return err
	}

	return tx.boltTx.Commit()
}

// View opens a database read transaction and executes the function f with the
// transaction passed as a parameter. After f exits, the transaction is rolled
// back. If f errors, its error is returned, not a rollback error (if any
// occur).
//
// NOTE: this is part of the PrivacyMapDB interface.
func (p *privacyMapDB) View(f func(tx PrivacyMapTx) error) error {
	tx, err := p.beginTx(false)
	if err != nil {
		return err
	}

	// Make sure the transaction rolls back in the event of a panic.
	defer func() {
		if tx != nil {
			_ = tx.boltTx.Rollback()
		}
	}()

	err = f(tx)
	rollbackErr := tx.boltTx.Rollback()
	if err != nil {
		return err
	}

	if rollbackErr != nil {
		return rollbackErr
	}
	return nil
}

// privacyMapTx is an implementation of PrivacyMapTx.
type privacyMapTx struct {
	*privacyMapDB
	boltTx *bbolt.Tx
}

// NewPair inserts a new real-pseudo pair into the db.
func (p *privacyMapTx) NewPair(real, pseudo string) error {
	privacyBucket, err := getBucket(p.boltTx, privacyBucketKey)
	if err != nil {
		return err
	}

	sessBucket, err := privacyBucket.CreateBucketIfNotExists(p.sessionID[:])
	if err != nil {
		return err
	}

	realToPseudoBucket, err := sessBucket.CreateBucketIfNotExists(
		realToPseudoKey,
	)
	if err != nil {
		return err
	}

	pseudoToRealBucket, err := sessBucket.CreateBucketIfNotExists(
		pseudoToRealKey,
	)
	if err != nil {
		return err
	}

	err = realToPseudoBucket.Put([]byte(real), []byte(pseudo))
	if err != nil {
		return err
	}

	return pseudoToRealBucket.Put([]byte(pseudo), []byte(real))
}

// PseudoToReal will check the db to see if the given pseudo key exists. If
// it does then the real value is returned, else an error is returned.
func (p *privacyMapTx) PseudoToReal(pseudo string) (string, error) {
	privacyBucket, err := getBucket(p.boltTx, privacyBucketKey)
	if err != nil {
		return "", err
	}

	sessBucket := privacyBucket.Bucket(p.sessionID[:])
	if sessBucket == nil {
		return "", ErrNoSuchKeyFound
	}

	pseudoToRealBucket := sessBucket.Bucket(pseudoToRealKey)
	if pseudoToRealBucket == nil {
		return "", ErrNoSuchKeyFound
	}

	real := pseudoToRealBucket.Get([]byte(pseudo))
	if len(real) == 0 {
		return "", ErrNoSuchKeyFound
	}

	return string(real), nil
}

// RealToPseudo will check the db to see if the given real key exists. If
// it does then the pseudo value is returned, else an error is returned.
func (p *privacyMapTx) RealToPseudo(real string) (string, error) {
	privacyBucket, err := getBucket(p.boltTx, privacyBucketKey)
	if err != nil {
		return "", err
	}

	sessBucket := privacyBucket.Bucket(p.sessionID[:])
	if sessBucket == nil {
		return "", ErrNoSuchKeyFound
	}

	realToPseudoBucket := sessBucket.Bucket(realToPseudoKey)
	if realToPseudoBucket == nil {
		return "", ErrNoSuchKeyFound
	}

	pseudo := realToPseudoBucket.Get([]byte(real))
	if len(pseudo) == 0 {
		return "", ErrNoSuchKeyFound
	}

	return string(pseudo), nil
}

func HideString(tx PrivacyMapTx, real string) (string, error) {
	pseudo, err := tx.RealToPseudo(real)
	if err != nil && err != ErrNoSuchKeyFound {
		return "", err
	}
	if err == nil {
		return pseudo, nil
	}

	pseudo, err = NewPseudoStr(len(real))
	if err != nil {
		return "", err
	}

	if err = tx.NewPair(real, pseudo); err != nil {
		return "", err
	}

	return pseudo, nil
}

func NewPseudoStr(n int) (string, error) {
	var max big.Int
	max.SetUint64(uint64(pseudoStrAlphabetLen))

	b := make([]rune, n)
	for i := range b {
		index, err := rand.Int(rand.Reader, &max)
		if err != nil {
			return "", err
		}

		b[i] = pseudoStrAlphabet[index.Uint64()]
	}

	return string(b), nil
}

func RevealString(tx PrivacyMapTx, pseudo string) (string, error) {
	if pseudo == "" {
		return pseudo, nil
	}

	return tx.PseudoToReal(pseudo)
}

func HideUint64(tx PrivacyMapTx, real uint64) (uint64, error) {
	str := Uint64ToStr(real)
	pseudo, err := tx.RealToPseudo(str)
	if err != nil && err != ErrNoSuchKeyFound {
		return 0, err
	}
	if err == nil {
		return StrToUint64(pseudo)
	}

	pseudoUint64, pseudoUint64Str := NewPseudoUint64()
	if err := tx.NewPair(str, pseudoUint64Str); err != nil {
		return 0, err
	}

	return pseudoUint64, nil
}

func RevealUint64(tx PrivacyMapTx, pseudo uint64) (uint64, error) {
	if pseudo == 0 {
		return 0, nil
	}

	real, err := tx.PseudoToReal(Uint64ToStr(pseudo))
	if err != nil {
		return 0, err
	}

	return StrToUint64(real)
}

func HideChanPoint(tx PrivacyMapTx, txid string, index uint32) (string,
	uint32, error) {

	cp := fmt.Sprintf("%s:%d", txid, index)
	pseudo, err := tx.RealToPseudo(cp)
	if err != nil && err != ErrNoSuchKeyFound {
		return "", 0, err
	}
	if err == nil {
		return decodeChannelPoint(pseudo)
	}

	newCp, err := NewPseudoChanPoint()
	if err != nil {
		return "", 0, err
	}

	if err := tx.NewPair(cp, newCp); err != nil {
		return "", 0, err
	}

	return decodeChannelPoint(newCp)
}

func NewPseudoChanPoint() (string, error) {
	pseudoTXID, err := NewPseudoStr(txidStringLen)
	if err != nil {
		return "", err
	}

	pseudoIndex := NewPseudoUint32()
	return fmt.Sprintf("%s:%d", pseudoTXID, pseudoIndex), nil
}

func RevealChanPoint(tx PrivacyMapTx, txid string, index uint32) (string,
	uint32, error) {

	fakePoint := fmt.Sprintf("%s:%d", txid, index)
	real, err := tx.PseudoToReal(fakePoint)
	if err != nil {
		return "", 0, err
	}

	return decodeChannelPoint(real)
}

func NewPseudoUint32() uint32 {
	b := make([]byte, 4)
	_, _ = rand.Read(b)

	return binary.BigEndian.Uint32(b)
}

func HideChanPointStr(tx PrivacyMapTx, cp string) (string, error) {
	txid, index, err := decodeChannelPoint(cp)
	if err != nil {
		return "", err
	}

	newTxid, newIndex, err := HideChanPoint(tx, txid, index)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("%s:%d", newTxid, newIndex), nil
}

func HideBytes(tx PrivacyMapTx, realBytes []byte) ([]byte, error) {
	real := hex.EncodeToString(realBytes)

	pseudo, err := HideString(tx, real)
	if err != nil {
		return nil, err
	}

	return hex.DecodeString(pseudo)
}

func RevealBytes(tx PrivacyMapTx, pseudoBytes []byte) ([]byte, error) {
	if pseudoBytes == nil {
		return nil, nil
	}

	pseudo := hex.EncodeToString(pseudoBytes)
	pseudo, err := RevealString(tx, pseudo)
	if err != nil {
		return nil, err
	}

	return hex.DecodeString(pseudo)
}

func NewPseudoUint64() (uint64, string) {
	b := make([]byte, 8)
	_, _ = rand.Read(b)

	i := binary.BigEndian.Uint64(b)

	return i, hex.EncodeToString(b)
}

func Uint64ToStr(i uint64) string {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, i)
	return hex.EncodeToString(b)
}

func StrToUint64(s string) (uint64, error) {
	b, err := hex.DecodeString(s)
	if err != nil {
		return 0, err
	}

	return binary.BigEndian.Uint64(b), nil
}

func decodeChannelPoint(cp string) (string, uint32, error) {
	parts := strings.Split(cp, ":")
	if len(parts) != 2 {
		return "", 0, fmt.Errorf("bad channel point encoding")
	}

	index, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return "", 0, err
	}

	return parts[0], uint32(index), nil
}
