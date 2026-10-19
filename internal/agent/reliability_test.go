package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestCrashedApplicationIsReportedWithoutRestart(t *testing.T) {
	h := newHarness(t)
	v := h.version("1")
	h.run(h.op("deploy", "1", &v))
	stale := h.deployment()
	h.sup.running["1"] = false
	starts := h.sup.starts
	for i := 0; i < 3; i++ {
		if err := h.e.observeOne(context.Background(), stale); err != nil {
			t.Fatal(err)
		}
		d := h.deployment()
		if h.sup.running["1"] || h.sup.starts != starts || d.Health != "unhealthy" || d.Error != "主进程未运行" {
			t.Fatal("crash was not reported or caused a restart", d)
		}
	}
	h.run(h.op("start", "1", &v))
	if !h.sup.running["1"] {
		t.Fatal("manual start failed")
	}
	h.run(h.op("stop", "1", nil))
	if err := h.e.observeOne(context.Background(), stale); err != nil {
		t.Fatal(err)
	}
	if h.sup.running["1"] || h.deployment().DesiredState != "stopped" {
		t.Fatal("stopped app restarted")
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

func TestApplicationHealthStatusControlsDeploymentWithoutRollback(t *testing.T) {
	for _, healthy := range []bool{true, false} {
		t.Run(fmt.Sprint(healthy), func(t *testing.T) {
			h := newHarness(t)
			old := h.version("1")
			h.run(h.op("deploy", "1", &old))
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !healthy {
					w.WriteHeader(503)
					fmt.Fprint(w, `{"error":"bad component checksum"}`)
					return
				}
				// A successful HTTP response is healthy even when initialization is still
				// in progress. No readiness/progress protocol is required or interpreted.
				fmt.Fprint(w, `{"ready":false,"status":"initializing","error":""}`)
			}))
			defer server.Close()
			next := h.version("2")
			op := h.op("deploy", "2", &next)
			op.Config.Health = model.Health{Type: "http", Target: server.URL, TimeoutSeconds: 2, StableSeconds: 1}
			result := h.run(op)
			if healthy {
				if result.Result != "succeeded" || h.deployment().Current != "2" || !h.sup.running["2"] {
					t.Fatal(result, h.deployment())
				}
				starts := h.sup.starts
				if err := h.e.observeOne(context.Background(), h.deployment()); err != nil {
					t.Fatal(err)
				}
				if h.deployment().Health != "healthy" || h.sup.starts != starts {
					t.Fatal("HTTP success caused unhealthy state or restart")
				}
			} else if result.Result != "failed" || h.deployment().Current != "2" || h.sup.running["1"] || !h.sup.running["2"] {
				t.Fatal(result, h.deployment())
			}
			if !healthy && (!strings.Contains(result.Error, "bad component checksum") || !strings.Contains(h.deployment().Error, "bad component checksum")) {
				t.Fatal("lost application error", result, h.deployment())
			}
		})
	}
}

func TestAgentStartupReportsStoppedAppWithoutStartingIt(t *testing.T) {
	h := newHarness(t)
	v := h.version("1")
	h.run(h.op("deploy", "1", &v))
	// Simulate a host reboot: committed running intent remains, process is gone.
	h.sup.running["1"] = false
	if err := h.db.Update(func(tx *store.Tx) error { return store.Put(tx, "settings", "boot_id", "previous-host-boot") }); err != nil {
		t.Fatal(err)
	}
	starts := h.sup.starts
	restarted := NewEngine(h.e.Root, h.e.NodeID, h.db, h.sup, h.e.Download)
	if err := restarted.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Boot(context.Background()); err != nil {
		t.Fatal(err)
	}
	d := h.deployment()
	if h.sup.starts != starts || h.sup.running["1"] || d.Current != "1" || d.RunState != "stopped" || d.Error != "主进程未运行" {
		t.Fatal("startup executed an unsolicited app start", d)
	}
	h.e = restarted
	if result := h.run(h.op("start", "1", &v)); result.Result != "succeeded" || !h.sup.running["1"] {
		t.Fatal("explicit administrator start failed", result)
	}
}

func TestExplicitStartDoesNotRestartRunningApplication(t *testing.T) {
	h := newHarness(t)
	v := h.version("1")
	h.run(h.op("deploy", "1", &v))
	// The real supervisor implements start as a no-op when its process is live.
	// Track stop calls so the engine cannot turn start into an implicit restart.
	sup := &stopCountingSupervisor{Interface: h.sup}
	h.e.Supervisor = sup
	if result := h.run(h.op("start", "1", &v)); result.Result != "succeeded" || sup.stops != 0 {
		t.Fatal(result, sup.stops)
	}
}

type stopCountingSupervisor struct {
	supervisor.Interface
	stops int
}

func (s *stopCountingSupervisor) Stop(ctx context.Context, sp supervisor.Spec, log func(string)) error {
	s.stops++
	return s.Interface.Stop(ctx, sp, log)
}
