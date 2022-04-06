package arcticdb

import (
	"math"
	"sync"

	"github.com/go-kit/log"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/atomic"

	"github.com/polarsignals/arcticdb/query/logicalplan"
)

type ColumnStore struct {
	mtx *sync.RWMutex
	dbs map[string]*DB
	reg prometheus.Registerer
}

func New(reg prometheus.Registerer) *ColumnStore {
	if reg == nil {
		reg = prometheus.NewRegistry()
	}

	return &ColumnStore{
		mtx: &sync.RWMutex{},
		dbs: map[string]*DB{},
		reg: reg,
	}
}

type DB struct {
	name string

	mtx    *sync.RWMutex
	tables map[string]*Table
	reg    prometheus.Registerer

	// Databases monotonically increasing transaction id
	txmtx *sync.RWMutex
	tx    *atomic.Uint64
	// active is the list of active transactions TODO: a gc goroutine should prune this list as parts get merged
	active map[uint64]uint64 // TODO probably not the best choice for active list...
}

func (s *ColumnStore) DB(name string) *DB {
	s.mtx.RLock()
	db, ok := s.dbs[name]
	s.mtx.RUnlock()
	if ok {
		return db
	}

	s.mtx.Lock()
	defer s.mtx.Unlock()

	// Need to double-check that in the meantime a database with the same name
	// wasn't concurrently created.
	db, ok = s.dbs[name]
	if ok {
		return db
	}

	db = &DB{
		name:   name,
		mtx:    &sync.RWMutex{},
		tables: map[string]*Table{},
		reg:    prometheus.WrapRegistererWith(prometheus.Labels{"db": name}, s.reg),

		active: map[uint64]uint64{},
		txmtx:  &sync.RWMutex{},
		tx:     atomic.NewUint64(0),
	}

	s.dbs[name] = db
	return db
}

func (db *DB) Table(name string, config *TableConfig, logger log.Logger) *Table {
	db.mtx.RLock()
	table, ok := db.tables[name]
	db.mtx.RUnlock()
	if ok {
		return table
	}

	db.mtx.Lock()
	defer db.mtx.Unlock()

	// Need to double-check that in the meantime another table with the same
	// name wasn't concurrently created.
	table, ok = db.tables[name]
	if ok {
		return table
	}

	table = newTable(db, name, config, db.reg, logger)
	db.tables[name] = table
	return table
}

func (db *DB) TableProvider() *DBTableProvider {
	return NewDBTableProvider(db)
}

type DBTableProvider struct {
	db *DB
}

func NewDBTableProvider(db *DB) *DBTableProvider {
	return &DBTableProvider{
		db: db,
	}
}

func (p *DBTableProvider) GetTable(name string) logicalplan.TableReader {
	p.db.mtx.RLock()
	defer p.db.mtx.RUnlock()
	return p.db.tables[name]
}

// beginRead starts a read transaction.
func (db *DB) beginRead() uint64 {
	return db.tx.Inc()
}

// begin is an internal function that Tables call to start a transaction for writes.
func (db *DB) begin() (uint64, func()) {
	tx := db.tx.Inc()
	db.txmtx.Lock()
	db.active[tx] = math.MaxUint64
	db.txmtx.Unlock()
	return tx, func() {
		// commit the transaction
		db.txmtx.Lock()
		db.active[tx] = db.tx.Inc()
		db.txmtx.Unlock()
	}
}

// txCompleted returns true if a write transaction has been completed.
func (db *DB) txCompleted(tx uint64) uint64 {
	db.txmtx.RLock()
	defer db.txmtx.RUnlock()

	finaltx, ok := db.active[tx]
	if !ok {
		return math.MaxUint64
	}

	return finaltx
}
