package main

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"deployer/internal/controller"
	"deployer/internal/identity"
	"deployer/internal/model"
	"deployer/internal/store"
	"golang.org/x/crypto/bcrypt"
)

func TestRestoreAndLocalAdminRecovery(t *testing.T) {
	root := filepath.Join(t.TempDir(), "original")
	s, err := controller.New(root, false, http.NotFoundHandler())
	if err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(t.TempDir(), "snapshot")
	if err = os.Mkdir(backup, 0700); err != nil {
		t.Fatal(err)
	}
	if err = s.DB.Update(func(tx *store.Tx) error {
		u, e := store.Get[model.User](tx, "users", "admin")
		if e != nil {
			return e
		}
		u.TOTPEnabled = true
		u.TOTPSecret = "test-secret"
		u.AuthVersion = 9
		if err := store.Put(tx, "nodes", "node", model.Node{ID: "node"}); err != nil {
			return err
		}
		if err := store.Put(tx, "batches", "batch", controller.Batch{ID: "batch", State: "running"}); err != nil {
			return err
		}
		return store.Put(tx, "users", u.ID, u)
	}); err != nil {
		t.Fatal(err)
	}
	if err = s.DB.Backup(filepath.Join(backup, "controller.db")); err != nil {
		t.Fatal(err)
	}
	if err = copyRegular(filepath.Join(root, "state", "master.key"), filepath.Join(backup, "master.key")); err != nil {
		t.Fatal(err)
	}
	if _, err = identity.OpenIssuer(filepath.Join(backup, "agent-issuer")); err != nil {
		t.Fatal(err)
	}
	if err = resetAdmin([]string{"--data", root, "--confirm"}); err == nil {
		t.Fatal("reset allowed while controller owns database")
	}
	s.Close()
	restored := filepath.Join(t.TempDir(), "restored")
	if err = restoreBackup([]string{"--backup", backup, "--data", restored}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"key.pem", "ca.pem"} {
		original, e := os.ReadFile(filepath.Join(backup, "agent-issuer", name))
		if e != nil {
			t.Fatal(e)
		}
		recovered, e := os.ReadFile(filepath.Join(restored, "state", "agent-issuer", name))
		if e != nil || !bytes.Equal(original, recovered) {
			t.Fatal("issuer not restored", e)
		}
	}
	if err = restoreBackup([]string{"--backup", backup, "--data", restored}); err == nil {
		t.Fatal("restore overwrote existing directory")
	}
	if err = resetAdmin([]string{"--data", restored, "--confirm"}); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(restored, "state", "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = db.View(func(tx *store.Tx) error {
		batch, err := store.Get[controller.Batch](tx, "batches", "batch")
		if err != nil || batch.State != "failed" {
			t.Fatal("snapshot batch can dispatch stale work", err)
		}
		marked, err := store.Get[bool](tx, "restored_nodes", "node")
		if err != nil || !marked {
			t.Fatal("restored node not quarantined", err)
		}
		restored, err := store.Get[bool](tx, "settings", "backup_restored")
		if err != nil || !restored {
			t.Fatal("missing restored-controller marker", err)
		}
		u, e := store.Get[model.User](tx, "users", "admin")
		if e != nil {
			return e
		}
		if u.TOTPEnabled || u.TOTPSecret != "" || u.AuthVersion != 10 {
			t.Fatalf("bad recovered account: %+v", u)
		}
		return bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(controller.DefaultAdminPassword))
	}); err != nil {
		t.Fatal(err)
	}
}
