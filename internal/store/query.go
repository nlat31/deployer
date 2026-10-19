package store

import (
	"encoding/json"
	"os"
	"strings"
	"time"
)

type Filter struct {
	Owner  string
	Scope  string
	States []string
	Limit  int
	Before int64
	Asc    bool
}

func Query[T any](t *Tx, kind string, f Filter) ([]T, error) {
	sql := "SELECT id,payload FROM records WHERE kind=?"
	args := []any{kind}
	if f.Owner != "" {
		sql += " AND owner=?"
		args = append(args, f.Owner)
	}
	if f.Scope != "" {
		sql += " AND scope=?"
		args = append(args, f.Scope)
	}
	if len(f.States) > 0 {
		sql += " AND state IN (" + strings.TrimSuffix(strings.Repeat("?,", len(f.States)), ",") + ")"
		for _, s := range f.States {
			args = append(args, s)
		}
	}
	if f.Before > 0 {
		sql += " AND updated_at<?"
		args = append(args, f.Before)
	}
	sql += " ORDER BY updated_at"
	if !f.Asc {
		sql += " DESC"
	}
	if f.Limit > 0 {
		sql += " LIMIT ?"
		args = append(args, f.Limit)
	}
	rows, e := t.tx.Query(sql, args...)
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
func Prune(t *Tx) error {
	day := time.Now().Add(-24 * time.Hour).UnixMilli()
	for _, kind := range []string{"sessions", "login_challenges", "totp_pending", "factor_attempts"} {
		if _, e := t.tx.Exec("DELETE FROM records WHERE kind=? AND updated_at<?", kind, day); e != nil {
			return e
		}
	}
	cutoff := time.Now().Add(-30 * 24 * time.Hour).UnixMilli()
	if _, e := t.tx.Exec("DELETE FROM events WHERE created_at<? AND operation_id NOT IN (SELECT id FROM records WHERE kind='operations' AND state IN ('active','needs_attention'))", cutoff); e != nil {
		return e
	}
	_, e := t.tx.Exec("DELETE FROM records WHERE kind='audit' AND id NOT IN (SELECT id FROM records WHERE kind='audit' ORDER BY updated_at DESC LIMIT 10000)")
	return e
}
func (s *Store) Checkpoint() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, e := s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	return e
}

// VACUUM INTO makes a consistent SQLite snapshot including committed WAL pages.
func (s *Store) Backup(path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.Exec("VACUUM INTO ?", path); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err = f.Chmod(0600); err != nil {
		return err
	}
	return f.Sync()
}
