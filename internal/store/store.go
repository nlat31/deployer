package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("record not found")

type Store struct {
	db   *sql.DB
	key  cipher.AEAD
	lock *os.File
	mu   sync.Mutex
}
type Tx struct {
	tx  *sql.Tx
	key cipher.AEAD
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("数据目录已被其他进程使用: %w", err)
	}
	fail := func(e error) (*Store, error) {
		syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		lock.Close()
		return nil, e
	}
	keyPath := filepath.Join(filepath.Dir(path), "master.key")
	key, err := os.ReadFile(keyPath)
	if os.IsNotExist(err) {
		if st, e := os.Stat(path); e == nil && st.Size() > 0 {
			return fail(fmt.Errorf("数据库存在但 master.key 丢失"))
		}
		key = make([]byte, 32)
		if _, err = rand.Read(key); err != nil {
			return fail(err)
		}
		if err = AtomicWrite(keyPath, key, 0600); err != nil {
			return fail(err)
		}
	} else if err != nil {
		return fail(err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return fail(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return fail(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return fail(err)
	}
	db.SetMaxOpenConns(1)
	var version int
	err = db.QueryRow("PRAGMA user_version").Scan(&version)
	if err != nil {
		db.Close()
		return fail(err)
	}
	if version != 0 && version != 3 {
		db.Close()
		return fail(fmt.Errorf("不支持的数据结构 %d；请使用独立的新开发数据目录", version))
	}
	_, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL; PRAGMA busy_timeout=5000;
CREATE TABLE IF NOT EXISTS records(kind TEXT NOT NULL,id TEXT NOT NULL,payload BLOB NOT NULL,updated_at INTEGER NOT NULL,owner TEXT NOT NULL,scope TEXT NOT NULL,state TEXT NOT NULL,PRIMARY KEY(kind,id));
CREATE INDEX IF NOT EXISTS records_recent ON records(kind,updated_at DESC);
CREATE INDEX IF NOT EXISTS records_lookup ON records(kind,owner,scope,state,updated_at);
CREATE TABLE IF NOT EXISTS events(operation_id TEXT NOT NULL,seq INTEGER NOT NULL,payload BLOB NOT NULL,created_at INTEGER NOT NULL,PRIMARY KEY(operation_id,seq));
CREATE INDEX IF NOT EXISTS events_age ON events(created_at);
PRAGMA user_version=3;`)
	if err != nil {
		db.Close()
		return fail(err)
	}
	_ = os.Chmod(path, 0600)
	return &Store{db: db, key: aead, lock: lock}, nil
}
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.db.Close()
	syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
	s.lock.Close()
	return err
}
func (s *Store) Update(fn func(*Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, e := s.db.Begin()
	if e != nil {
		return e
	}
	defer t.Rollback()
	if e = fn(&Tx{t, s.key}); e != nil {
		return e
	}
	return t.Commit()
}
func (s *Store) View(fn func(*Tx) error) error { return s.Update(fn) }
func encrypt(key cipher.AEAD, b []byte, aad string) ([]byte, error) {
	nonce := make([]byte, key.NonceSize())
	if _, e := rand.Read(nonce); e != nil {
		return nil, e
	}
	return key.Seal(nonce, nonce, b, []byte(aad)), nil
}
func decrypt(key cipher.AEAD, b []byte, aad string) ([]byte, error) {
	n := key.NonceSize()
	if len(b) < n {
		return nil, fmt.Errorf("损坏的加密记录")
	}
	return key.Open(nil, b[:n], b[n:], []byte(aad))
}
func Get[T any](t *Tx, kind, id string) (T, error) {
	var out T
	var b []byte
	e := t.tx.QueryRow("SELECT payload FROM records WHERE kind=? AND id=?", kind, id).Scan(&b)
	if errors.Is(e, sql.ErrNoRows) {
		return out, ErrNotFound
	}
	if e != nil {
		return out, e
	}
	b, e = decrypt(t.key, b, kind+":"+id)
	if e != nil {
		return out, e
	}
	e = json.Unmarshal(b, &out)
	return out, e
}
func List[T any](t *Tx, kind string) ([]T, error) {
	rows, e := t.tx.Query("SELECT id,payload FROM records WHERE kind=? ORDER BY id", kind)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []T{}
	for rows.Next() {
		var id string
		var b []byte
		if e = rows.Scan(&id, &b); e != nil {
			return nil, e
		}
		b, e = decrypt(t.key, b, kind+":"+id)
		if e != nil {
			return nil, e
		}
		var v T
		if e = json.Unmarshal(b, &v); e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func Put(t *Tx, kind, id string, v any) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	b, e = encrypt(t.key, b, kind+":"+id)
	if e != nil {
		return e
	}
	owner, scope, state := "", "", ""
	if v, ok := v.(interface {
		StorageIndex() (string, string, string)
	}); ok {
		owner, scope, state = v.StorageIndex()
	}
	_, e = t.tx.Exec("INSERT INTO records VALUES(?,?,?,?,?,?,?) ON CONFLICT(kind,id) DO UPDATE SET payload=excluded.payload,updated_at=excluded.updated_at,owner=excluded.owner,scope=excluded.scope,state=excluded.state", kind, id, b, time.Now().UnixMilli(), owner, scope, state)
	return e
}
func Delete(t *Tx, kind, id string) error {
	_, e := t.tx.Exec("DELETE FROM records WHERE kind=? AND id=?", kind, id)
	return e
}
func AddEvent(t *Tx, op string, seq int64, v any) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	b, e = encrypt(t.key, b, fmt.Sprintf("event:%s:%d", op, seq))
	if e != nil {
		return e
	}
	_, e = t.tx.Exec("INSERT OR IGNORE INTO events VALUES(?,?,?,?)", op, seq, b, time.Now().UnixMilli())
	return e
}
func Events[T any](t *Tx, op string, after int64, limit int) ([]T, error) {
	rows, e := t.tx.Query("SELECT seq,payload FROM events WHERE operation_id=? AND seq>? ORDER BY seq LIMIT ?", op, after, limit)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []T{}
	for rows.Next() {
		var seq int64
		var b []byte
		if e = rows.Scan(&seq, &b); e != nil {
			return nil, e
		}
		b, e = decrypt(t.key, b, fmt.Sprintf("event:%s:%d", op, seq))
		if e != nil {
			return nil, e
		}
		var v T
		if e = json.Unmarshal(b, &v); e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func LastSeq(t *Tx, op string) (int64, error) {
	var n int64
	e := t.tx.QueryRow("SELECT COALESCE(MAX(seq),0) FROM events WHERE operation_id=?", op).Scan(&n)
	return n, e
}
func TrimEvents(t *Tx, op string, keep int) error {
	_, e := t.tx.Exec("DELETE FROM events WHERE operation_id=? AND seq <= (SELECT COALESCE(MAX(seq),0)-? FROM events WHERE operation_id=?)", op, keep, op)
	return e
}
func AtomicWrite(path string, b []byte, mode os.FileMode) error {
	f, e := os.CreateTemp(filepath.Dir(path), ".write-")
	if e != nil {
		return e
	}
	name := f.Name()
	defer os.Remove(name)
	if e = f.Chmod(mode); e == nil {
		_, e = f.Write(b)
	}
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		return e
	}
	if e = os.Rename(name, path); e != nil {
		return e
	}
	return SyncDir(filepath.Dir(path))
}
func SyncDir(path string) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}
