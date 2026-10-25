package agent

import (
	"context"
	"deployer/internal/model"
	"deployer/internal/store"
	"deployer/internal/supervisor"
	"testing"
	"time"
)

func cancelResult(t *testing.T, h *harness, id string) model.Operation {
	t.Helper()
	var o model.Operation
	if err := h.db.View(func(tx *store.Tx) error {
		var err error
		o, err = store.Get[model.Operation](tx, "operations", id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if o.Result != "cancelled" {
		t.Fatal(o)
	}
	return o
}
func TestCancelWhileDownloadingDoesNotStartApp(t *testing.T) {
	h := newHarness(t)
	v := h.version("1")
	o := h.op("deploy", "1", &v)
	if _, err := h.e.Accept(o); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	h.e.Download = func(ctx context.Context, _ model.Version, _ string) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	}
	done := make(chan error, 1)
	go func() { done <- h.e.Run(context.Background(), o.ID) }()
	<-entered
	if err := h.e.RequestCancel(o.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancel did not interrupt download")
	}
	cancelResult(t, h, o.ID)
	if h.sup.starts != 0 {
		t.Fatal("cancelled download started app")
	}
}

type cancelSetup struct {
	supervisor.Interface
	entered chan struct{}
}

func (s cancelSetup) Hook(ctx context.Context, sp supervisor.Spec, script string, log func(string)) error {
	if script == "setup.sh" {
		close(s.entered)
		<-ctx.Done()
		return ctx.Err()
	}
	return s.Interface.Hook(ctx, sp, script, log)
}
func TestCancelSetupPreservesRunningPreviousVersion(t *testing.T) {
	h := newHarness(t)
	old := h.version("1")
	h.run(h.op("deploy", "1", &old))
	next := h.version("2")
	o := h.op("deploy", "2", &next)
	entered := make(chan struct{})
	h.e.Supervisor = cancelSetup{h.sup, entered}
	if _, err := h.e.Accept(o); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- h.e.Run(context.Background(), o.ID) }()
	<-entered
	if err := h.e.RequestCancel(o.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("setup was not cancelled")
	}
	cancelResult(t, h, o.ID)
	if !h.sup.running["1"] || h.sup.running["2"] || h.sup.starts != 1 {
		t.Fatal("cancellation switched/restarted app")
	}
}
func TestCancelledTaskRemainsCancelledAcrossRecovery(t *testing.T) {
	h := newHarness(t)
	v := h.version("1")
	o := h.op("deploy", "1", &v)
	if _, err := h.e.Accept(o); err != nil {
		t.Fatal(err)
	}
	if err := h.e.RequestCancel(o.ID); err != nil {
		t.Fatal(err)
	}
	restarted := NewEngine(h.e.Root, "node", h.db, h.sup, h.e.Download)
	if err := restarted.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	cancelResult(t, h, o.ID)
	if h.sup.starts != 0 || h.sup.setupCount != 0 {
		t.Fatal("cancelled task resumed after restart")
	}
}

type cancelStart struct {
	supervisor.Interface
	entered chan struct{}
}

func (s cancelStart) Start(ctx context.Context, sp supervisor.Spec) error {
	close(s.entered)
	<-ctx.Done()
	return ctx.Err()
}
func TestCancelDuringStartRequiresManualAttentionWithoutRestart(t *testing.T) {
	h := newHarness(t)
	old := h.version("1")
	h.run(h.op("deploy", "1", &old))
	next := h.version("2")
	o := h.op("deploy", "2", &next)
	entered := make(chan struct{})
	h.e.Supervisor = cancelStart{h.sup, entered}
	if _, err := h.e.Accept(o); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- h.e.Run(context.Background(), o.ID) }()
	<-entered
	if err := h.e.RequestCancel(o.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("start not cancelled")
	}
	h.db.View(func(tx *store.Tx) error {
		saved, _ := store.Get[model.Operation](tx, "operations", o.ID)
		if saved.Result != "needs_attention" {
			t.Fatal(saved)
		}
		return nil
	})
	if h.sup.starts != 1 || h.deployment().DesiredState != "stopped" {
		t.Fatal("cancel automatically restarted app")
	}
}

type cancelQuiesce struct {
	supervisor.Interface
	calls int
}

func (s *cancelQuiesce) Quiesce(context.Context, supervisor.Spec) error { s.calls++; return nil }
func TestCancelledRecoveryQuiescesOrphanHooksBeforeFinishing(t *testing.T) {
	h := newHarness(t)
	v := h.version("1")
	o := h.op("deploy", "1", &v)
	if _, err := h.e.Accept(o); err != nil {
		t.Fatal(err)
	}
	if err := h.e.RequestCancel(o.ID); err != nil {
		t.Fatal(err)
	}
	q := &cancelQuiesce{Interface: h.sup}
	restarted := NewEngine(h.e.Root, "node", h.db, q, h.e.Download)
	if err := restarted.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	cancelResult(t, h, o.ID)
	if q.calls != 1 || h.sup.starts != 0 {
		t.Fatal("cancelled recovery did not clean orphan hooks", q.calls)
	}
}
