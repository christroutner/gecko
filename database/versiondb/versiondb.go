// (c) 2019-2020, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package versiondb

import (
	"sort"
	"strings"
	"sync"

	"github.com/ava-labs/gecko/database"
	"github.com/ava-labs/gecko/database/memdb"
	"github.com/ava-labs/gecko/database/nodb"
)

// Database implements the Database interface by living on top of another
// database, writing changes to the underlying database only when commit is
// called.
type Database struct {
	lock sync.RWMutex
	mem  map[string]valueDelete
	db   database.Database
}

type valueDelete struct {
	value  []byte
	delete bool
}

// New returns a new prefixed database
func New(db database.Database) *Database {
	return &Database{
		mem: make(map[string]valueDelete, memdb.DefaultSize),
		db:  db,
	}
}

// Has implements the database.Database interface
func (db *Database) Has(key []byte) (bool, error) {
	db.lock.RLock()
	defer db.lock.RUnlock()

	if db.mem == nil {
		return false, database.ErrClosed
	}
	if val, has := db.mem[string(key)]; has {
		return !val.delete, nil
	}
	return db.db.Has(key)
}

// Get implements the database.Database interface
func (db *Database) Get(key []byte) ([]byte, error) {
	db.lock.RLock()
	defer db.lock.RUnlock()

	if db.mem == nil {
		return nil, database.ErrClosed
	}
	if val, has := db.mem[string(key)]; has {
		if val.delete {
			return nil, database.ErrNotFound
		}
		return copyBytes(val.value), nil
	}
	return db.db.Get(key)
}

// Put implements the database.Database interface
func (db *Database) Put(key, value []byte) error {
	db.lock.Lock()
	defer db.lock.Unlock()

	if db.mem == nil {
		return database.ErrClosed
	}
	db.mem[string(key)] = valueDelete{value: value}
	return nil
}

// Delete implements the database.Database interface
func (db *Database) Delete(key []byte) error {
	db.lock.Lock()
	defer db.lock.Unlock()

	if db.mem == nil {
		return database.ErrClosed
	}
	db.mem[string(key)] = valueDelete{delete: true}
	return nil
}

// NewBatch implements the database.Database interface
func (db *Database) NewBatch() database.Batch { return &batch{db: db} }

// NewIterator implements the database.Database interface
func (db *Database) NewIterator() database.Iterator { return db.NewIteratorWithStartAndPrefix(nil, nil) }

// NewIteratorWithStart implements the database.Database interface
func (db *Database) NewIteratorWithStart(start []byte) database.Iterator {
	return db.NewIteratorWithStartAndPrefix(start, nil)
}

// NewIteratorWithPrefix implements the database.Database interface
func (db *Database) NewIteratorWithPrefix(prefix []byte) database.Iterator {
	return db.NewIteratorWithStartAndPrefix(nil, prefix)
}

// NewIteratorWithStartAndPrefix implements the database.Database interface
func (db *Database) NewIteratorWithStartAndPrefix(start, prefix []byte) database.Iterator {
	db.lock.RLock()
	defer db.lock.RUnlock()

	if db.mem == nil {
		return &nodb.Iterator{Err: database.ErrClosed}
	}

	startString := string(start)
	prefixString := string(prefix)
	keys := make([]string, 0, len(db.mem))
	for key := range db.mem {
		if strings.HasPrefix(key, prefixString) && key >= startString {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys) // Keys need to be in sorted order
	values := make([]valueDelete, 0, len(keys))
	for _, key := range keys {
		values = append(values, db.mem[key])
	}

	return &iterator{
		Iterator: db.db.NewIteratorWithStartAndPrefix(start, prefix),
		keys:     keys,
		values:   values,
	}
}

// Stat implements the database.Database interface
func (db *Database) Stat(stat string) (string, error) {
	db.lock.RLock()
	defer db.lock.RUnlock()

	if db.mem == nil {
		return "", database.ErrClosed
	}
	return db.db.Stat(stat)
}

// Compact implements the database.Database interface
func (db *Database) Compact(start, limit []byte) error {
	db.lock.Lock()
	defer db.lock.Unlock()

	if db.mem == nil {
		return database.ErrClosed
	}
	return db.db.Compact(start, limit)
}

// SetDatabase changes the underlying database to the specified database
func (db *Database) SetDatabase(newDB database.Database) error {
	db.lock.Lock()
	defer db.lock.Unlock()

	if db.mem == nil {
		return database.ErrClosed
	}

	db.db = newDB
	return nil
}

// GetDatabase returns the underlying database
func (db *Database) GetDatabase() database.Database {
	db.lock.RLock()
	defer db.lock.RUnlock()

	return db.db
}

// Commit writes all the operations of this database to the underlying database
func (db *Database) Commit() error {
	db.lock.Lock()
	defer db.lock.Unlock()

	if db.mem == nil {
		return database.ErrClosed
	}
	if len(db.mem) == 0 {
		return nil
	}

	batch := db.db.NewBatch()
	for key, value := range db.mem {
		if value.delete {
			if err := batch.Delete([]byte(key)); err != nil {
				return err
			}
		} else if err := batch.Put([]byte(key), value.value); err != nil {
			return err
		}
	}
	if err := batch.Write(); err != nil {
		return err
	}

	db.mem = make(map[string]valueDelete, memdb.DefaultSize)
	return nil
}

// Close implements the database.Database interface
func (db *Database) Close() error {
	db.lock.Lock()
	defer db.lock.Unlock()

	if db.mem == nil {
		return database.ErrClosed
	}
	db.mem = nil
	db.db = nil
	return nil
}

type keyValue struct {
	key    []byte
	value  []byte
	delete bool
}

type batch struct {
	db     *Database
	writes []keyValue
	size   int
}

// Put implements the Database interface
func (b *batch) Put(key, value []byte) error {
	b.writes = append(b.writes, keyValue{copyBytes(key), copyBytes(value), false})
	b.size += len(value)
	return nil
}

// Delete implements the Database interface
func (b *batch) Delete(key []byte) error {
	b.writes = append(b.writes, keyValue{copyBytes(key), nil, true})
	b.size++
	return nil
}

// ValueSize implements the Database interface
func (b *batch) ValueSize() int { return b.size }

// Write implements the Database interface
func (b *batch) Write() error {
	b.db.lock.Lock()
	defer b.db.lock.Unlock()

	if b.db.mem == nil {
		return database.ErrClosed
	}

	for _, kv := range b.writes {
		b.db.mem[string(kv.key)] = valueDelete{
			value:  kv.value,
			delete: kv.delete,
		}
	}
	return nil
}

// Reset implements the Database interface
func (b *batch) Reset() {
	b.writes = b.writes[:0]
	b.size = 0
}

// Replay implements the Database interface
func (b *batch) Replay(w database.KeyValueWriter) error {
	for _, kv := range b.writes {
		if kv.delete {
			if err := w.Delete(kv.key); err != nil {
				return err
			}
		} else if err := w.Put(kv.key, kv.value); err != nil {
			return err
		}
	}
	return nil
}

// iterator walks over both the in memory database and the underlying database
// at the same time.
type iterator struct {
	database.Iterator

	key, value []byte

	keys   []string
	values []valueDelete

	initialized, exhausted bool
}

// Next moves the iterator to the next key/value pair. It returns whether the
// iterator is exhausted. We must pay careful attention to set the proper values
// based on if the in memory db or the underlying db should be read next
func (it *iterator) Next() bool {
	if !it.initialized {
		it.exhausted = !it.Iterator.Next()
		it.initialized = true
	}

	for {
		switch {
		case it.exhausted && len(it.keys) == 0:
			it.key = nil
			it.value = nil
			return false
		case it.exhausted:
			nextKey := it.keys[0]
			nextValue := it.values[0]

			it.keys = it.keys[1:]
			it.values = it.values[1:]

			if !nextValue.delete {
				it.key = []byte(nextKey)
				it.value = nextValue.value
				return true
			}
		case len(it.keys) == 0:
			it.key = it.Iterator.Key()
			it.value = it.Iterator.Value()
			it.exhausted = !it.Iterator.Next()
			return true
		default:
			memKey := it.keys[0]
			memValue := it.values[0]

			dbKey := it.Iterator.Key()

			dbStringKey := string(dbKey)
			switch {
			case memKey < dbStringKey:
				it.keys = it.keys[1:]
				it.values = it.values[1:]

				if !memValue.delete {
					it.key = []byte(memKey)
					it.value = memValue.value
					return true
				}
			case dbStringKey < memKey:
				it.key = dbKey
				it.value = it.Iterator.Value()
				it.exhausted = !it.Iterator.Next()
				return true
			default:
				it.keys = it.keys[1:]
				it.values = it.values[1:]
				it.exhausted = !it.Iterator.Next()

				if !memValue.delete {
					it.key = []byte(memKey)
					it.value = memValue.value
					return true
				}
			}
		}
	}
}

// Key implements the Iterator interface
func (it *iterator) Key() []byte { return it.key }

// Value implements the Iterator interface
func (it *iterator) Value() []byte { return it.value }

// Release implements the Iterator interface
func (it *iterator) Release() {
	it.key = nil
	it.value = nil
	it.keys = nil
	it.values = nil
	it.Iterator.Release()
}

func copyBytes(bytes []byte) []byte {
	copiedBytes := make([]byte, len(bytes))
	copy(copiedBytes, bytes)
	return copiedBytes
}
