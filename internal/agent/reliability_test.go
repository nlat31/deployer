package agent

import (
	"context"
	"fmt"
	"testing"

	"deployer/internal/model"
	"deployer/internal/store"
	"deployer/internal/supervisor"
)

type partialClean struct{ supervisor.Interface }

func (s partialClean) Hook(ctx context.Context, sp supervisor.Spec, script string, log func(string)) error {
	if err := s.Interface.Hook(ctx, sp, script, log); err != nil {
		return err
	}
	if script == "clean.sh" {
		return fmt.Errorf("interrupted after partial cleanup")
	}
	return nil
}
func TestInterruptedCleanupForcesSetupBeforeRestart(t *testing.T) {
	h := newHarness(t)
	v := h.version("1")
	h.run(h.op("deploy", "1", &v))
	h.run(h.op("stop", "1", nil))
	before := h.sup.setupCount
	h.e.Supervisor = partialClean{h.sup}
	if o := h.run(h.op("clean_data", "1", nil)); o.Result != "failed" {
		t.Fatal(o)
	}
	h.e.Supervisor = h.sup
	if o := h.run(h.op("start", "1", &v)); o.Result != "succeeded" {
		t.Fatal(o)
	}
	if h.sup.setupCount != before+1 {
		t.Fatal("partial cleanup was treated as initialized")
	}
}

func TestCrashRecoveryBackoffAndStoppedState(t *testing.T) {
	h := newHarness(t)
	v := h.version("1")
	h.run(h.op("deploy", "1", &v))
	stale := h.deployment()
	h.sup.running["1"] = false
	if err := h.e.observeOne(context.Background(), stale); err != nil {
		t.Fatal(err)
	}
	d := h.deployment()
	if !h.sup.running["1"] || d.RestartFailures != 1 || d.NextRestartAt <= model.Now() {
		t.Fatalf("not recovered: %+v", d)
	}
	h.sup.running["1"] = false
	starts := h.sup.starts
	if err := h.e.observeOne(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if h.sup.starts != starts {
		t.Fatal("ignored restart backoff")
	}
	h.run(h.op("stop", "1", nil))
	if err := h.e.observeOne(context.Background(), stale); err != nil {
		t.Fatal(err)
	}
	if h.sup.running["1"] || h.deployment().DesiredState != "stopped" {
		t.Fatal("stale observation restarted stopped app")
	}
}

func TestAcceptedTaskBlocksStaleObservation(t *testing.T) {
	h := newHarness(t)
	v := h.version("1")
	h.run(h.op("deploy", "1", &v))
	stale := h.deployment()
	o := h.op("stop", "1", nil)
	if _, err := h.e.Accept(o); err != nil {
		t.Fatal(err)
	}
	if err := h.e.observeOne(context.Background(), stale); err != nil {
		t.Fatal(err)
	}
	d := h.deployment()
	if d.Revision != o.Revision || d.DesiredState != "stopped" {
		t.Fatalf("accepted state overwritten: %+v", d)
	}
	if err := h.e.Run(context.Background(), o.ID); err != nil {
		t.Fatal(err)
	}
	if err := h.db.View(func(tx *store.Tx) error {
		out, err := store.Get[model.Operation](tx, "outbox", o.ID)
		if err == nil && !out.Terminal() {
			t.Fatal("nonterminal outbox")
		}
		return err
	}); err != nil {
		t.Fatal("terminal result not durably queued", err)
	}
}
