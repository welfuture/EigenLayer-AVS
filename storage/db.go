package storage

import (
	"fmt"
	"os"
	"strings"

	badger "github.com/dgraph-io/badger/v4"
)

type Config struct {
	Path string
}

type Sequence interface {
	Next() (uint64, error)
	Release() error
}

type Storage interface {
	Setup() error
	Close() error

	GetSequence(prefix []byte, inflightItem uint64) (Sequence, error)

	Exist(key []byte) (bool, error)
	GetKey(key []byte) ([]byte, error)
	GetByPrefix(prefix []byte) ([]*KeyValueItem, error)
	GetKeyHasPrefix(prefix []byte) ([][]byte, error)
	FirstKVHasPrefix(prefix []byte) ([]byte, []byte, error)

	// A key only operation that returns key that has a prefix
	ListKeys(prefix string) ([]string, error)
	ListKeysMulti(prefixes []string) ([]string, error)

	// A key only counting keys that has a prefix, very efficient because only operating on lsm tree
	CountKeysByPrefix(prefix []byte) (int64, error)
	CountKeysByPrefixes(prefixes [][]byte) (int64, error)

	BatchWrite(updates map[string][]byte) error
	Move(src, dest []byte) error
	Set(key, value []byte) error
	Delete(key []byte) error

	Vacuum() error

	DbPath() string
}

type KeyValueItem struct {
	Key   []byte
	Value []byte
}

type BadgerStorage struct {
	config *Config
	db     *badger.DB
	seqs   []*badger.Sequence
}

// Create storage pool at the particular path
func NewWithPath(path string) (Storage, error) {
	return New(&Config{
		Path: path,
	})
}

// Create storage pool with the given config
func New(c *Config) (Storage, error) {
	opts := badger.DefaultOptions(c.Path)
	db, err := badger.Open(
		opts.WithSyncWrites(true),
	)

	if err != nil {
		return nil, err
	}

	return &BadgerStorage{
		config: c,
		db:     db,

		seqs: make([]*badger.Sequence, 0),
	}, nil
}

func (s *BadgerStorage) Setup() error {
	return nil
}

func (s *BadgerStorage) Close() error {
	for _, seq := range s.seqs {
		seq.Release()
	}
	return s.db.Close()
}

func (s *BadgerStorage) BatchWrite(updates map[string][]byte) error {
	txn := s.db.NewTransaction(true)
	for k, v := range updates {
		if err := txn.Set([]byte(k), v); err == badger.ErrTxnTooBig {
			_ = txn.Commit()
			txn = s.db.NewTransaction(true)
			_ = txn.Set([]byte(k), []byte(v))
		}
	}
	_ = txn.Commit()

	return nil
}

func (s *BadgerStorage) Set(key, value []byte) error {
	return s.db.Update(func(txn *badger.Txn) error {
		err := txn.Set(key, value)
		return err
	})
}

func (s *BadgerStorage) Delete(key []byte) error {
	return s.db.Update(func(txn *badger.Txn) error {
		err := txn.Delete(key)
		return err
	})
}

// GetByPrefix return a list of key/value item whoser key prefix matches
func (s *BadgerStorage) GetByPrefix(prefix []byte) ([]*KeyValueItem, error) {
	var result []*KeyValueItem

	err := s.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchSize = 30
		it := txn.NewIterator(opts)
		defer it.Close()
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			item := it.Item()

			k := item.KeyCopy(nil)
			v, e := item.ValueCopy(nil)
			if e != nil {
				return e
			}

			result = append(result, &KeyValueItem{
				Key:   k,
				Value: v,
			})
		}
		return nil
	})

	if err != nil {
		return result, err
	}

	return result, nil
}

func (s *BadgerStorage) GetKeyHasPrefix(prefix []byte) ([][]byte, error) {
	var result [][]byte

	err := s.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchValues = false

		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			item := it.Item()
			k := item.KeyCopy(nil)

			result = append(result, k)
		}
		return nil
	})

	if err != nil {
		return result, err
	}

	return result, nil
}

// CountKeysByPrefix return total key under a specfic prefix
func (s *BadgerStorage) CountKeysByPrefix(prefix []byte) (int64, error) {
	total := int64(0)

	if len(prefix) == 0 {
		return 0, fmt.Errorf("cannot count prefix with length 0")
	}

	err := s.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchValues = false

		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			total += 1
		}
		return nil
	})

	if err != nil {
		return 0, err
	}

	return total, nil
}

func (s *BadgerStorage) CountKeysByPrefixes(prefixes [][]byte) (int64, error) {
	total := int64(0)

	for _, prefix := range prefixes {
		count, err := s.CountKeysByPrefix(prefix)
		if err != nil {
			return 0, err
		}
		total += count
	}

	return total, nil
}

func (s *BadgerStorage) Exist(key []byte) (bool, error) {
	found := false
	err := s.db.View(func(txn *badger.Txn) error {
		_, err := txn.Get(key)
		if err != nil {
			return err
		}

		found = true
		return nil
	})

	return found, err
}

func (s *BadgerStorage) GetKey(key []byte) ([]byte, error) {
	var value []byte

	err := s.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(key)
		if err != nil {
			return err
		}

		err = item.Value(func(val []byte) error {
			value = append([]byte{}, val...)
			return nil
		})

		return err
	})

	return value, err
}

// Wrap badgerdb sequence
func (s *BadgerStorage) GetSequence(prefix []byte, inflightItem uint64) (Sequence, error) {
	seq, e := s.db.GetSequence(prefix, inflightItem)
	if e != nil {
		return nil, e
	}

	s.seqs = append(s.seqs, seq)
	return seq, nil
}

func (s *BadgerStorage) FirstKVHasPrefix(prefix []byte) ([]byte, []byte, error) {
	var k []byte
	var v []byte

	err := s.db.View(func(txn *badger.Txn) error {
		itOpts := badger.DefaultIteratorOptions
		itOpts.PrefetchValues = true
		itOpts.PrefetchSize = 1
		it := txn.NewIterator(itOpts)

		// go to smallest key after prefix
		it.Seek(prefix)
		defer it.Close()
		// iteration done, no item found
		if !it.ValidForPrefix(prefix) {
			return nil
		}

		item := it.Item()

		k = item.KeyCopy(nil)

		var err error
		v, err = item.ValueCopy(nil)
		return err
	})

	if err == nil {
		return k, v, nil
	}

	return nil, nil, err
}

func (s *BadgerStorage) Move(src []byte, dest []byte) error {
	return s.db.Update(func(txn *badger.Txn) error {
		item, err := txn.Get(src)
		if err != nil {
			return err
		}

		b, err := item.ValueCopy(nil)
		if err != nil {
			return err
		}

		// key is found, we will delete from source, then set on target
		err = txn.Delete(src)
		if err != nil {
			return err
		}

		// create in Dest queue
		err = txn.Set(dest, b)
		return err
	})
}

func (a *BadgerStorage) ListKeys(prefix string) ([]string, error) {
	var keys []string

	if prefix == "*" {
		prefix = ""
	} else if strings.HasSuffix(prefix, "*") {
		prefix = prefix[:len(prefix)-1]
	}

	err := a.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchValues = false
		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek([]byte(prefix)); it.ValidForPrefix([]byte(prefix)); it.Next() {
			item := it.Item()
			key := item.KeyCopy(nil)

			keys = append(keys, fmt.Sprintf("%s", key))
		}
		return nil

	})
	if err == nil {
		return keys, nil
	}

	return nil, err
}

// ListKeys from multiple suffix. This is similar to a join in RDBMS
func (a *BadgerStorage) ListKeysMulti(prefixes []string) ([]string, error) {
	var keys []string

	for _, prefix := range prefixes {
		if len(prefix) == 0 {
			continue
		}

		data, err := a.ListKeys(prefix)
		if err != nil {
			continue
		}
		keys = append(keys, data...)
	}

	return keys, nil
}

func (a *BadgerStorage) Vacuum() error {
	return a.db.RunValueLogGC(0.7)
}

func (a *BadgerStorage) DbPath() string {
	return a.config.Path
}

// Destroy is destructive action that shutdown a database, and wipe out its entire data directory
func Destroy(a *BadgerStorage) error {
	a.Close()
	return os.RemoveAll(a.config.Path)
}
