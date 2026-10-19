package agent

import (
	"context"
	"deployer/internal/model"
	"deployer/internal/store"
	"os"
	"path/filepath"
	"testing"
)

func TestPurgeRunningVersionKeepsOtherVersions(t *testing.T) {
	h := newHarness(t)
	v1 := h.version("1")
	h.run(h.op("deploy", "1", &v1))
	v2 := h.version("2")
	h.run(h.op("deploy", "2", &v2))
	result := h.run(h.op("remove_version", "2", nil))
	if result.Result != "succeeded" {
		t.Fatal(result)
	}
	if _, err := os.Stat(filepath.Join(h.e.Root, "apps/sample/versions/2")); !os.IsNotExist(err) {
		t.Fatal("version still exists", err)
	}
	if _, err := os.Stat(filepath.Join(h.e.Root, "apps/sample/versions/1/data/seed")); err != nil {
		t.Fatal("other version lost", err)
	}
	d := h.deployment()
	if d.Current != "" || d.RunningVersion != "" || h.sup.running["2"] {
		t.Fatal(d)
	}
	h.run(h.op("remove_version", "1", nil))
	if _, err := os.Stat(filepath.Join(h.e.Root, "apps/sample")); !os.IsNotExist(err) {
		t.Fatal("empty app directory remains", err)
	}
}
func TestPurgeAppStopsAllAndResumes(t *testing.T) {
	h := newHarness(t)
	v := h.version("1")
	h.run(h.op("deploy", "1", &v))
	o := h.op("purge_app", "", nil)
	if _, err := h.e.Accept(o); err != nil {
		t.Fatal(err)
	}
	// Crash after removing files but before the inventory transaction.
	if err := h.sup.Stop(context.Background(), h.e.spec(o, "1", o.Config), func(string) {}); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(h.e.Root, "apps/sample")); err != nil {
		t.Fatal(err)
	}
	if err := h.e.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := h.e.Observe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(h.e.Root, "apps/sample")); !os.IsNotExist(err) {
		t.Fatal("recreated deleted directory", err)
	}
	h.db.View(func(tx *store.Tx) error {
		items, err := store.List[model.Installation](tx, "installations")
		if len(items) != 0 {
			t.Fatal(items)
		}
		return err
	})
}
func TestPurgeStopFailurePreservesDataAndRetry(t *testing.T) {
	h := newHarness(t)
	v := h.version("1")
	h.run(h.op("deploy", "1", &v))
	h.sup.failStop = true
	o := h.run(h.op("detach", "", nil))
	if o.Result != "failed" {
		t.Fatal(o)
	}
	if _, err := os.Stat(filepath.Join(h.e.Root, "apps/sample/versions/1/data/seed")); err != nil {
		t.Fatal(err)
	}
	if h.deployment().DesiredState != "stopped" {
		t.Fatal("self heal still enabled")
	}
	h.sup.failStop = false
	o = h.run(h.op("detach", "", nil))
	if o.Result != "succeeded" {
		t.Fatal(o)
	}
	if _, err := os.Stat(filepath.Join(h.e.Root, "apps/sample")); !os.IsNotExist(err) {
		t.Fatal(err)
	}
}
func TestPurgeRejectsLinkedVersionDirectory(t *testing.T) {
	h := newHarness(t)
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "keep"), []byte("safe"), 0600)
	os.MkdirAll(filepath.Join(h.e.Root, "apps/sample/versions"), 0700)
	os.Symlink(outside, filepath.Join(h.e.Root, "apps/sample/versions/1"))
	o := h.run(h.op("purge_app", "", nil))
	if o.Result != "failed" {
		t.Fatal(o)
	}
	if _, err := os.Stat(filepath.Join(outside, "keep")); err != nil {
		t.Fatal(err)
	}
}
