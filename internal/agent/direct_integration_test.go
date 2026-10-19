package agent

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"deployer/internal/model"
	"deployer/internal/packages"
	"deployer/internal/supervisor"
)

func realVersion(t *testing.T, h *harness, name string, broken bool) model.Version {
	t.Helper()
	v := h.version(name)
	f, err := os.Create(h.archives[name])
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	start := "exec sleep 300\n"
	if broken {
		start = "exit 9\n"
	}
	for script, body := range map[string]string{
		"setup.sh": "echo initialized > \"$APP_DATA_DIR/seed\"\n",
		"start.sh": start,
		"stop.sh":  "kill -TERM \"$APP_MAIN_PID\" 2>/dev/null || true\n",
		"clean.sh": "find \"$APP_DATA_DIR\" -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +\n",
	} {
		b := []byte("#!/bin/bash\nset -eu\n" + body)
		if err = tw.WriteHeader(&tar.Header{Name: script, Mode: 0700, Size: int64(len(b))}); err != nil {
			t.Fatal(err)
		}
		if _, err = tw.Write(b); err != nil {
			t.Fatal(err)
		}
	}
	if err = tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err = gz.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()
	v.Digest, err = packages.Digest(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(f.Name())
	v.Size = info.Size()
	return v
}
func TestDirectEngineDeploySwitchRecoverStopClean(t *testing.T) {
	h := newHarness(t)
	sup := &supervisor.Direct{}
	h.e.Supervisor = sup
	ctx := context.Background()
	versions := []model.Version{realVersion(t, h, "1", false), realVersion(t, h, "2", false), realVersion(t, h, "bad", true)}
	sp := func(v string) supervisor.Spec {
		return supervisor.Spec{Root: h.e.Root, AppID: "sample", Version: v, OperationID: model.ID()}
	}
	t.Cleanup(func() {
		for _, v := range versions {
			if err := sup.Stop(ctx, sp(v.Version), func(string) {}); err != nil {
				t.Error(err)
			}
		}
	})
	if o := h.run(h.op("deploy", "1", &versions[0])); o.Result != "succeeded" {
		t.Fatal(o)
	}
	before, err := sup.Status(ctx, sp("1"))
	if err != nil {
		t.Fatal(err)
	}
	// Recreate the engine and supervisor using persisted state, with the process alive.
	h.e = NewEngine(h.e.Root, h.e.NodeID, h.db, &supervisor.Direct{}, h.e.Download)
	if err = h.e.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if err = h.e.Observe(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := sup.Status(ctx, sp("1"))
	if err != nil || after.PID != before.PID {
		t.Fatal("restart duplicated or lost process", after, err)
	}
	if o := h.run(h.op("switch", "bad", &versions[2])); o.Result != "failed_recovered" {
		t.Fatal("rollback", o)
	}
	if o := h.run(h.op("switch", "2", &versions[1])); o.Result != "succeeded" {
		t.Fatal(o)
	}
	old, _ := sup.Status(ctx, sp("1"))
	if old.Running {
		t.Fatal("old version remained running")
	}
	current, err := sup.Status(ctx, sp("2"))
	if err != nil {
		t.Fatal(err)
	}
	syscall.Kill(current.PID, syscall.SIGKILL)
	deadline := time.Now().Add(time.Second)
	for {
		st, _ := sup.Status(ctx, sp("2"))
		if !st.Running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("killed process still reported running")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err = h.e.Observe(ctx); err != nil {
		t.Fatal(err)
	}
	restored, _ := sup.Status(ctx, sp("2"))
	if !restored.Running || restored.PID == current.PID {
		t.Fatal("automatic recovery failed", h.deployment())
	}
	if o := h.run(h.op("stop", "2", nil)); o.Result != "succeeded" {
		t.Fatal(o)
	}
	if err = h.e.Observe(ctx); err != nil {
		t.Fatal(err)
	}
	st, _ := sup.Status(ctx, sp("2"))
	if st.Running {
		t.Fatal("explicit stop automatically restarted")
	}
	if o := h.run(h.op("clean_data", "2", nil)); o.Result != "succeeded" {
		t.Fatal(o)
	}
	if _, err = os.Stat(filepath.Join(sp("2").Dir(), "data", "seed")); !os.IsNotExist(err) {
		t.Fatal("data not cleaned", err)
	}
	if o := h.run(h.op("start", "2", &versions[1])); o.Result != "succeeded" {
		t.Fatal(o)
	}
	if _, err = os.Stat(filepath.Join(sp("2").Dir(), "data", "seed")); err != nil {
		t.Fatal("setup did not run after clean", err)
	}
}

func TestDirectPurgeRunningAppRemovesProcessAndDirectory(t *testing.T) {
	h := newHarness(t)
	sup := &supervisor.Direct{}
	h.e.Supervisor = sup
	v := realVersion(t, h, "1", false)
	sp := supervisor.Spec{Root: h.e.Root, AppID: "sample", Version: "1", OperationID: model.ID()}
	t.Cleanup(func() { _ = sup.Stop(context.Background(), sp, func(string) {}) })
	if o := h.run(h.op("deploy", "1", &v)); o.Result != "succeeded" {
		t.Fatal(o)
	}
	before, err := sup.Status(context.Background(), sp)
	if err != nil || !before.Running {
		t.Fatal(before, err)
	}
	if o := h.run(h.op("purge_app", "", nil)); o.Result != "succeeded" {
		t.Fatal(o)
	}
	if err := syscall.Kill(before.PID, 0); err == nil {
		t.Fatal("deleted live process directory")
	}
	if _, err := os.Stat(filepath.Join(h.e.Root, "apps/sample")); !os.IsNotExist(err) {
		t.Fatal("app remains", err)
	}
}
