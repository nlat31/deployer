package agent

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"deployer/internal/model"
	"deployer/internal/packages"
	"deployer/internal/store"
	"deployer/internal/supervisor"
)

type fakeSupervisor struct {
	mu         sync.Mutex
	running    map[string]bool
	failStart  map[string]bool
	setupCount int
	cleanCount int
	starts     int
	maxRunning int
	failStop   bool
}

func newFake() *fakeSupervisor {
	return &fakeSupervisor{running: map[string]bool{}, failStart: map[string]bool{}}
}
func (f *fakeSupervisor) Prepare(s supervisor.Spec) error { return nil }
func (f *fakeSupervisor) Hook(ctx context.Context, s supervisor.Spec, script string, log func(string)) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch script {
	case "setup.sh":
		f.setupCount++
		if strings.Contains(s.Version, "bad-setup") {
			return fmt.Errorf("intentional setup failure")
		}
		return os.WriteFile(filepath.Join(s.Dir(), "data", "seed"), []byte(s.Version), 0600)
	case "clean.sh":
		f.cleanCount++
		entries, e := os.ReadDir(filepath.Join(s.Dir(), "data"))
		if e != nil {
			return e
		}
		for _, en := range entries {
			if e = os.RemoveAll(filepath.Join(s.Dir(), "data", en.Name())); e != nil {
				return e
			}
		}
	}
	return nil
}
func (f *fakeSupervisor) Start(ctx context.Context, s supervisor.Spec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failStart[s.Version] {
		return fmt.Errorf("intentional start failure")
	}
	f.running[s.Version] = true
	f.starts++
	n := 0
	for _, on := range f.running {
		if on {
			n++
		}
	}
	if n > f.maxRunning {
		f.maxRunning = n
	}
	return nil
}
func (f *fakeSupervisor) Stop(ctx context.Context, s supervisor.Spec, log func(string)) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failStop {
		return fmt.Errorf("cannot verify stopped processes")
	}
	f.running[s.Version] = false
	return nil
}

func TestReconcileRequiresConfirmedStopAndPreservesData(t *testing.T) {
	h := newHarness(t)
	v := h.version("1")
	h.run(h.op("deploy", v.Version, &v))
	h.sup.failStop = true
	failed := h.run(h.op("stop", v.Version, nil))
	if failed.Result != "needs_attention" {
		t.Fatal(failed)
	}
	if _, err := h.e.Accept(h.op("start", v.Version, &v)); err == nil {
		t.Fatal("allowed conflicting start while state unknown")
	}
	h.sup.failStop = false
	resolved := h.run(h.op("reconcile", v.Version, nil))
	if resolved.Result != "succeeded" || h.deployment().RunState != "stopped" {
		t.Fatal(resolved)
	}
	if _, err := os.Stat(filepath.Join(h.e.Root, "apps", "sample", "versions", "1", "data", "seed")); err != nil {
		t.Fatal("reconcile removed data", err)
	}
	if o := h.run(h.op("start", v.Version, &v)); o.Result != "succeeded" {
		t.Fatal("did not release blocked App", o)
	}
}
func (f *fakeSupervisor) Status(ctx context.Context, s supervisor.Spec) (supervisor.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.running[s.Version] {
		return supervisor.Status{Running: true, PID: 123, State: "active"}, nil
	}
	return supervisor.Status{State: "inactive"}, nil
}
func (f *fakeSupervisor) Logs(ctx context.Context, s supervisor.Spec) (string, error) {
	return "sample log", nil
}

type harness struct {
	e        *Engine
	db       *store.Store
	sup      *fakeSupervisor
	archives map[string]string
	t        *testing.T
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{"state", "cache", "staging", "apps"} {
		os.Mkdir(filepath.Join(root, d), 0755)
	}
	db, err := store.Open(filepath.Join(root, "state", "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	h := &harness{db: db, sup: newFake(), archives: map[string]string{}, t: t}
	h.e = NewEngine(root, "node", db, h.sup, func(ctx context.Context, v model.Version, dest string) error {
		src, err := os.Open(h.archives[v.Version])
		if err != nil {
			return err
		}
		defer src.Close()
		out, err := os.Create(dest)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, src)
		out.Close()
		return err
	})
	return h
}
func (h *harness) version(version string) model.Version {
	f, e := os.CreateTemp(h.t.TempDir(), "*.tgz")
	if e != nil {
		h.t.Fatal(e)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, s := range []string{"setup.sh", "start.sh", "stop.sh", "clean.sh"} {
		b := []byte("#!/bin/bash\nexit 0\n")
		tw.WriteHeader(&tar.Header{Name: s, Mode: 0755, Size: int64(len(b))})
		tw.Write(b)
	}
	tw.Close()
	gz.Close()
	f.Close()
	digest, _ := packages.Digest(f.Name())
	st, _ := os.Stat(f.Name())
	h.archives[version] = f.Name()
	return model.Version{AppID: "sample", Version: version, OS: "linux", Arch: runtime.GOARCH, Digest: digest, Size: st.Size(), Status: "available"}
}
func (h *harness) deployment() model.Deployment {
	var d model.Deployment
	h.db.View(func(tx *store.Tx) error { d, _ = store.Get[model.Deployment](tx, "deployments", "sample"); return nil })
	return d
}
func (h *harness) op(action, version string, artifact *model.Version) model.Operation {
	d := h.deployment()
	c := d.Config
	if c.ID == "" {
		c = model.Config{ID: model.ID(), Health: model.Health{Type: "process", TimeoutSeconds: 2, StableSeconds: 1}, AutoRollback: true}
	}
	o := model.Operation{ID: model.ID(), NodeID: "node", AppID: "sample", Action: action, Version: version, Artifact: artifact, ExpectedRevision: d.Revision, Revision: d.Revision + 1, Config: c, Start: true}
	h.db.View(func(tx *store.Tx) error {
		i, _ := store.Get[model.Installation](tx, "installations", model.Key("sample", version))
		o.DataEpoch = i.DataEpoch
		return nil
	})
	return o
}
func (h *harness) run(o model.Operation) model.Operation {
	h.t.Helper()
	fresh, e := h.e.Accept(o)
	if e != nil || !fresh {
		h.t.Fatalf("accept: %v %v", fresh, e)
	}
	if e = h.e.Run(context.Background(), o.ID); e != nil {
		h.t.Fatal(e)
	}
	h.db.View(func(tx *store.Tx) error { o, _ = store.Get[model.Operation](tx, "operations", o.ID); return nil })
	return o
}
func TestDeploySwitchFailureAndNoDoubleInstance(t *testing.T) {
	h := newHarness(t)
	a := h.version("1.0")
	b := h.version("2.0")
	bad := h.version("bad-setup")
	o := h.run(h.op("deploy", a.Version, &a))
	if o.Result != "succeeded" {
		t.Fatal(o)
	}
	if h.deployment().Current != a.Version {
		t.Fatal("wrong current")
	}
	if h.run(h.op("switch", bad.Version, &bad)).Result != "failed" || !h.sup.running[a.Version] {
		t.Fatal("setup failure affected old version")
	}
	h.sup.failStart[b.Version] = true
	o = h.run(h.op("switch", b.Version, &b))
	if o.Result != "failed_recovered" || h.deployment().Current != a.Version || !h.sup.running[a.Version] {
		t.Fatal("failed to recover", o)
	}
	h.sup.failStart[b.Version] = false
	o = h.run(h.op("switch", b.Version, &b))
	if o.Result != "succeeded" || h.deployment().Previous != a.Version {
		t.Fatal("switch failed", o)
	}
	if h.sup.maxRunning != 1 {
		t.Fatal("overlapping instances")
	}
	bdata, _ := os.ReadFile(filepath.Join(h.e.Root, "apps", "sample", "versions", b.Version, "data", "seed"))
	adata, _ := os.ReadFile(filepath.Join(h.e.Root, "apps", "sample", "versions", a.Version, "data", "seed"))
	if string(adata) != a.Version || string(bdata) != b.Version {
		t.Fatal("version data crossed")
	}
}
func TestCleanDeduplicationAndProtectedDeletion(t *testing.T) {
	h := newHarness(t)
	a := h.version("1")
	h.run(h.op("deploy", a.Version, &a))
	if o := h.run(h.op("clean_data", a.Version, nil)); o.Result != "failed" {
		t.Fatal("cleaned running version")
	}
	h.run(h.op("stop", a.Version, nil))
	clean := h.op("clean_data", a.Version, nil)
	if o := h.run(clean); o.Result != "succeeded" {
		t.Fatal(o)
	}
	if fresh, e := h.e.Accept(clean); e != nil || fresh {
		t.Fatal("duplicate accepted", e)
	}
	h.e.Run(context.Background(), clean.ID)
	if h.sup.cleanCount != 1 {
		t.Fatal("clean replayed")
	}
	h.run(h.op("start", a.Version, &a))
	if h.sup.setupCount != 2 {
		t.Fatal("did not initialize after clean")
	}
	h.run(h.op("stop", a.Version, nil))
	stale := h.op("clean_data", a.Version, nil)
	stale.DataEpoch = clean.DataEpoch
	if o := h.run(stale); o.Result != "failed" {
		t.Fatal("stale clean accepted")
	}
	if _, e := os.Stat(filepath.Join(h.e.Root, "apps", "sample", "versions", a.Version, "data", "seed")); e != nil {
		t.Fatal("stale clean deleted new data")
	}
	if o := h.run(h.op("remove_version", a.Version, nil)); o.Result != "succeeded" {
		t.Fatal(o)
	}
	if _, err := os.Stat(filepath.Join(h.e.Root, "apps/sample")); !os.IsNotExist(err) {
		t.Fatal("removed application directory remains", err)
	}
	// Repeated deletion is safe even after all files and inventory disappeared.
	if o := h.run(h.op("remove_version", a.Version, nil)); o.Result != "succeeded" {
		t.Fatal(o)
	}

}
func TestRecoveryAfterStartBeforeCommit(t *testing.T) {
	h := newHarness(t)
	a := h.version("1")
	b := h.version("2")
	h.run(h.op("deploy", a.Version, &a))
	o := h.op("switch", b.Version, &b)
	fresh, err := h.e.Accept(o)
	if err != nil || !fresh {
		t.Fatal(err)
	}
	h.db.View(func(tx *store.Tx) error { o, _ = store.Get[model.Operation](tx, "operations", o.ID); return nil })
	if err = h.e.prepare(context.Background(), &o, func(string) {}); err != nil {
		t.Fatal(err)
	}
	h.sup.Stop(context.Background(), h.e.spec(o, a.Version, o.Before.Config), func(string) {})
	h.e.phase(&o, "checking")
	h.sup.Start(context.Background(), h.e.spec(o, b.Version, o.Config))
	restarted := NewEngine(h.e.Root, "node", h.db, h.sup, h.e.Download)
	if err = restarted.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	var final model.Operation
	h.db.View(func(tx *store.Tx) error { final, _ = store.Get[model.Operation](tx, "operations", o.ID); return nil })
	if final.Result != "failed_recovered" || h.deployment().Current != a.Version || h.sup.running[b.Version] || !h.sup.running[a.Version] {
		t.Fatal("unsafe recovery", final)
	}
}
