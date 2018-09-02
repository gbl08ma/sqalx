package sqalx

import (
	"database/sql"
	"errors"
	"strings"
	"sync"

	"github.com/jmoiron/sqlx"
	uuid "github.com/satori/go.uuid"
)

var (
	// ErrNotInTransaction is returned when using Commit
	// outside of a transaction.
	ErrNotInTransaction = errors.New("not in transaction")

	// ErrIncompatibleOption is returned when using an option incompatible
	// with the selected driver.
	ErrIncompatibleOption = errors.New("incompatible option")
)

// A Node is a database driver that can manage nested transactions.
type Node interface {
	Driver

	// Close the underlying sqlx connection.
	Close() error
	// Begin a new transaction.
	Beginx() (Node, error)
	// Rollback the associated transaction.
	Rollback() error
	// Commit the assiociated transaction.
	Commit() error
	// Tx returns the underlying transaction.
	Tx() *sqlx.Tx

	// Load returns the value stored in the map for a key, or nil if no
	// value is present.
	// The ok result indicates whether value was found in the map.
	Load(key interface{}) (value interface{}, ok bool)
	// Store sets the value for a key.
	Store(key, value interface{})
	// LoadOrStore returns the existing value for the key if present.
	// Otherwise, it stores and returns the given value.
	// The loaded result is true if the value was loaded, false if stored.
	LoadOrStore(key, value interface{}) (actual interface{}, loaded bool)
	// Delete deletes the value for a key.
	Delete(key interface{})
	// Range calls f sequentially for each key and value present in the map.
	// If f returns false, range stops the iteration.
	//
	// Range does not necessarily correspond to any consistent snapshot of the Map's
	// contents: no key will be visited more than once, but if the value for any key
	// is stored or deleted concurrently, Range may reflect any mapping for that key
	// from any point during the Range call.
	//
	// Range may be O(N) with the number of elements in the map even if f returns
	// false after a constant number of calls.
	Range(f func(key, value interface{}) bool)
}

// A Driver can query the database. It can either be a *sqlx.DB or a *sqlx.Tx
// and therefore is limited to the methods they have in common.
type Driver interface {
	sqlx.Execer
	sqlx.Queryer
	sqlx.Preparer
	BindNamed(query string, arg interface{}) (string, []interface{}, error)
	DriverName() string
	Get(dest interface{}, query string, args ...interface{}) error
	MustExec(query string, args ...interface{}) sql.Result
	NamedExec(query string, arg interface{}) (sql.Result, error)
	NamedQuery(query string, arg interface{}) (*sqlx.Rows, error)
	PrepareNamed(query string) (*sqlx.NamedStmt, error)
	Preparex(query string) (*sqlx.Stmt, error)
	Rebind(query string) string
	Select(dest interface{}, query string, args ...interface{}) error
}

// New creates a new Node with the given DB.
func New(db *sqlx.DB, options ...Option) (Node, error) {
	n := node{
		db:     db,
		Driver: db,
		smap:   new(sync.Map),
	}

	for _, opt := range options {
		err := opt(&n)
		if err != nil {
			return nil, err
		}
	}

	return &n, nil
}

// NewFromTransaction creates a new Node from the given transaction.
func NewFromTransaction(tx *sqlx.Tx, options ...Option) (Node, error) {
	n := node{
		tx:     tx,
		Driver: tx,
		smap:   new(sync.Map),
	}

	for _, opt := range options {
		err := opt(&n)
		if err != nil {
			return nil, err
		}
	}

	return &n, nil
}

// Connect to a database.
func Connect(driverName, dataSourceName string, options ...Option) (Node, error) {
	db, err := sqlx.Connect(driverName, dataSourceName)
	if err != nil {
		return nil, err
	}

	node, err := New(db, options...)
	if err != nil {
		// the connection has been opened within this function, we must close it
		// on error.
		db.Close()
		return nil, err
	}

	return node, nil
}

type node struct {
	Driver
	db               *sqlx.DB
	tx               *sqlx.Tx
	smap             *sync.Map
	savePointID      string
	savePointEnabled bool
	nested           bool
}

func (n *node) Close() error {
	return n.db.Close()
}

func (n node) Beginx() (Node, error) {
	var err error

	switch {
	case n.tx == nil:
		// new actual transaction
		n.tx, err = n.db.Beginx()
		// values are scoped to each transaction
		n.smap = new(sync.Map)
		n.Driver = n.tx
	case n.savePointEnabled:
		// already in a transaction: using savepoints
		n.nested = true
		// savepoints name must start with a char and cannot contain dashes (-)
		n.savePointID = "sp_" + strings.Replace(uuid.Must(uuid.NewV1()).String(), "-", "_", -1)
		_, err = n.tx.Exec("SAVEPOINT " + n.savePointID)
	default:
		// already in a transaction: reusing current transaction
		n.nested = true
	}

	if err != nil {
		return nil, err
	}

	return &n, nil
}

func (n *node) Rollback() error {
	if n.tx == nil {
		return nil
	}

	var err error

	if n.savePointEnabled && n.savePointID != "" {
		_, err = n.tx.Exec("ROLLBACK TO SAVEPOINT " + n.savePointID)
	} else if !n.nested {
		err = n.tx.Rollback()
	}

	if err != nil {
		return err
	}

	n.tx = nil
	n.Driver = nil

	return nil
}

func (n *node) Commit() error {
	if n.tx == nil {
		return ErrNotInTransaction
	}

	var err error

	if n.savePointID != "" {
		_, err = n.tx.Exec("RELEASE SAVEPOINT " + n.savePointID)
	} else if !n.nested {
		err = n.tx.Commit()
	}

	if err != nil {
		return err
	}

	n.tx = nil
	n.Driver = nil

	return nil
}

// Tx returns the underlying transaction.
func (n *node) Tx() *sqlx.Tx {
	return n.tx
}

// Load returns the value stored in the map for a key, or nil if no
// value is present.
// The ok result indicates whether value was found in the map.
func (n *node) Load(key interface{}) (value interface{}, ok bool) {
	return n.smap.Load(key)
}

// Store sets the value for a key.
func (n *node) Store(key, value interface{}) {
	n.smap.Store(key, value)
}

// LoadOrStore returns the existing value for the key if present.
// Otherwise, it stores and returns the given value.
// The loaded result is true if the value was loaded, false if stored.
func (n *node) LoadOrStore(key, value interface{}) (actual interface{}, loaded bool) {
	return n.smap.LoadOrStore(key, value)
}

// Delete deletes the value for a key.
func (n *node) Delete(key interface{}) {
	n.smap.Delete(key)
}

// Range calls f sequentially for each key and value present in the map.
// If f returns false, range stops the iteration.
//
// Range does not necessarily correspond to any consistent snapshot of the Map's
// contents: no key will be visited more than once, but if the value for any key
// is stored or deleted concurrently, Range may reflect any mapping for that key
// from any point during the Range call.
//
// Range may be O(N) with the number of elements in the map even if f returns
// false after a constant number of calls.
func (n *node) Range(f func(key, value interface{}) bool) {
	n.smap.Range(f)
}

// Option to configure sqalx
type Option func(*node) error

// SavePoint option enables PostgreSQL Savepoints for nested transactions.
func SavePoint(enabled bool) Option {
	return func(n *node) error {
		if enabled && n.Driver.DriverName() != "postgres" {
			return ErrIncompatibleOption
		}
		n.savePointEnabled = enabled
		return nil
	}
}
