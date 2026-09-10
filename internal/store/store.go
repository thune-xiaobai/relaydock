// Package store is a small SQLite-backed local journal, shared by the two roles.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"

	"github.com/gofrs/flock"
	_ "modernc.org/sqlite"
)

var ErrMissing = errors.New("record not found")

type Store struct {
	db   *sql.DB
	lock *flock.Flock
	mu   sync.Mutex
}
type Tx struct{ tx *sql.Tx }

func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	l := flock.New(filepath.Join(dir, "process.lock"))
	ok, err := l.TryLock()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("state directory is already in use")
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "state.db"))
	if err != nil {
		l.Unlock()
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL; PRAGMA busy_timeout=5000;
CREATE TABLE IF NOT EXISTS kv(bucket TEXT NOT NULL, key TEXT NOT NULL, value BLOB NOT NULL, PRIMARY KEY(bucket,key));`)
	if err != nil {
		db.Close()
		l.Unlock()
		return nil, err
	}
	_ = os.Chmod(filepath.Join(dir, "state.db"), 0600)
	return &Store{db: db, lock: l}, nil
}
func (s *Store) Close() error { err := s.db.Close(); _ = s.lock.Unlock(); return err }
func (s *Store) Update(fn func(*Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = fn(&Tx{tx}); err != nil {
		return err
	}
	return tx.Commit()
}
func (t *Tx) Get(bucket, key string, v any) error {
	var b []byte
	err := t.tx.QueryRow("SELECT value FROM kv WHERE bucket=? AND key=?", bucket, key).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrMissing
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
func (t *Tx) Put(bucket, key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = t.tx.Exec("INSERT INTO kv(bucket,key,value) VALUES(?,?,?) ON CONFLICT(bucket,key) DO UPDATE SET value=excluded.value", bucket, key, b)
	return err
}
func (t *Tx) Delete(bucket, key string) error {
	_, err := t.tx.Exec("DELETE FROM kv WHERE bucket=? AND key=?", bucket, key)
	return err
}
func (t *Tx) List(bucket string) (map[string]json.RawMessage, error) {
	rows, err := t.tx.Query("SELECT key,value FROM kv WHERE bucket=? ORDER BY key", bucket)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string]json.RawMessage{}
	for rows.Next() {
		var k string
		var b []byte
		if err = rows.Scan(&k, &b); err != nil {
			return nil, err
		}
		m[k] = append([]byte(nil), b...)
	}
	return m, rows.Err()
}
func (s *Store) Get(bucket, key string, v any) error {
	return s.Update(func(t *Tx) error { return t.Get(bucket, key, v) })
}
func (s *Store) Put(bucket, key string, v any) error {
	return s.Update(func(t *Tx) error { return t.Put(bucket, key, v) })
}
func (s *Store) Delete(bucket, key string) error {
	return s.Update(func(t *Tx) error { return t.Delete(bucket, key) })
}
func (s *Store) List(bucket string) (map[string]json.RawMessage, error) {
	var m map[string]json.RawMessage
	err := s.Update(func(t *Tx) error { var e error; m, e = t.List(bucket); return e })
	return m, err
}
