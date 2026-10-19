package store

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestTransactionsEncryptionAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "test.db")
	s, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	if other, e := Open(path); e == nil {
		other.Close()
		t.Fatal("second writer opened")
	}
	secret := "secret-value-that-must-not-appear-in-sqlite"
	if e = s.Update(func(tx *Tx) error { return Put(tx, "config", "a", secret) }); e != nil {
		t.Fatal(e)
	}
	sentinel := errors.New("rollback")
	if e = s.Update(func(tx *Tx) error {
		if e := Put(tx, "config", "a", "wrong"); e != nil {
			return e
		}
		return sentinel
	}); !errors.Is(e, sentinel) {
		t.Fatal(e)
	}
	s.Close()
	raw, _ := os.ReadFile(path)
	if bytes.Contains(raw, []byte(secret)) {
		t.Fatal("plaintext credential in database")
	}
	s, e = Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if e = s.View(func(tx *Tx) error {
		v, e := Get[string](tx, "config", "a")
		if e == nil && v != secret {
			t.Fatalf("lost transaction: %q", v)
		}
		return e
	}); e != nil {
		t.Fatal(e)
	}
}
