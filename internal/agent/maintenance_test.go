package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"deployer/internal/model"
	"deployer/internal/store"
	"deployer/internal/testpki"
)

func TestMaintenancePreservesInstalledPackagesActiveStagingAndBusinessData(t *testing.T) {
	pki := testpki.New(t)
	cert := pki.Node(t, "test-node")
	c, err := Open(t.TempDir(), "https://127.0.0.1:1", TLSFiles{CA: cert.CA, Cert: cert.Cert, Key: cert.Key}, newFake())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	pinned, orphan, recent := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
	old := time.Now().Add(-8 * 24 * time.Hour)
	write := func(rel string, aged bool) {
		p := filepath.Join(c.Root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("preserve"), 0600); err != nil {
			t.Fatal(err)
		}
		if aged {
			if err := os.Chtimes(p, old, old); err != nil {
				t.Fatal(err)
			}
		}
	}
	write("cache/"+pinned+".tar.gz", true)
	write("cache/"+orphan+".tar.gz", true)
	write("cache/"+recent+".tar.gz", false)
	write("apps/app/versions/1/data/customer-data", true)
	for _, name := range []string{"active-task", "abandoned-task"} {
		write("staging/"+name+"/partial", true)
		if err := os.Chtimes(filepath.Join(c.Root, "staging", name), old, old); err != nil {
			t.Fatal(err)
		}
	}
	if err = c.DB.Update(func(tx *store.Tx) error {
		if err := store.Put(tx, "installations", model.Key("app", "1"), model.Installation{AppID: "app", Version: "1", Digest: pinned}); err != nil {
			return err
		}
		return store.Put(tx, "operations", "active-task", model.Operation{ID: "active-task", AppID: "app", NodeID: c.Identity.NodeID})
	}); err != nil {
		t.Fatal(err)
	}
	if err = c.maintain(); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"cache/" + pinned + ".tar.gz", "cache/" + recent + ".tar.gz", "staging/active-task/partial", "apps/app/versions/1/data/customer-data"} {
		if _, err := os.Stat(filepath.Join(c.Root, p)); err != nil {
			t.Fatal("maintenance deleted protected file", p, err)
		}
	}
	for _, p := range []string{"cache/" + orphan + ".tar.gz", "staging/abandoned-task"} {
		if _, err := os.Stat(filepath.Join(c.Root, p)); !os.IsNotExist(err) {
			t.Fatal("unused file not reclaimed", p, err)
		}
	}
}
