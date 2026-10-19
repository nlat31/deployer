package launcher

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"deployer/internal/selfupdate"
)

func TestMain(m *testing.M) {
	if os.Getenv("LAUNCHER_TEST_CHILD") == "1" {
		// Copies of the helper executable have distinct digests via an appended byte.
		exe, _ := os.Executable()
		b, _ := os.ReadFile(exe)
		mode := b[len(b)-1]
		if mode == 'B' {
			os.Exit(8)
		}
		if mode == 'O' {
			_ = Pulse("ready")
		}
		for {
			if mode != 'H' && mode != 'O' {
				phase := "ready"
				if Draining() {
					phase = "drained"
				}
				_ = Pulse(phase)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	stablePeriod = 350 * time.Millisecond
	trialTimeout = 900 * time.Millisecond
	candidateDelay = 50 * time.Millisecond
	restartDelay = 10 * time.Millisecond
	os.Exit(m.Run())
}
func fixture(t *testing.T) (Runtime, string, string) {
	t.Helper()
	r := Runtime{Root: t.TempDir(), Role: "agent"}
	os.MkdirAll(filepath.Join(r.Root, "config"), 0700)
	os.MkdirAll(r.Dir(), 0700)
	os.WriteFile(filepath.Join(r.Root, "config", "agent.env"), []byte("LAUNCHER_TEST_CHILD=1\n"), 0600)
	exe, _ := os.Executable()
	b, e := os.ReadFile(exe)
	if e != nil {
		t.Fatal(e)
	}
	stage := func(mode byte) string {
		p := filepath.Join(t.TempDir(), "helper")
		if e := os.WriteFile(p, append(b, mode), 0700); e != nil {
			t.Fatal(e)
		}
		d, e := r.stage(p)
		if e != nil {
			t.Fatal(e)
		}
		return d
	}
	old := stage('A')
	good := stage('G')
	if e = r.save(State{Current: old, Phase: "committed"}); e != nil {
		t.Fatal(e)
	}
	if e = r.link(old); e != nil {
		t.Fatal(e)
	}
	return r, old, good
}
func wait(t *testing.T, fn func() bool) {
	t.Helper()
	until := time.Now().Add(8 * time.Second)
	for time.Now().Before(until) {
		if fn() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition timed out")
}
func run(t *testing.T, r Runtime) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- r.Run() }()
	t.Cleanup(func() {
		_ = selfupdate.Atomic(r.file("stop"), []byte("stop"), 0600)
		select {
		case e := <-done:
			if e != nil {
				t.Error(e)
			}
		case <-time.After(4 * time.Second):
			var p Process
			_ = selfupdate.Load(r.file("child.json"), &p)
			_ = p.Signal(syscall.SIGKILL)
			t.Error("launcher did not stop")
		}
	})
}
func TestUpgradeCommitAndStop(t *testing.T) {
	r, old, good := fixture(t)
	run(t, r)
	wait(t, func() bool { return r.alive("child.json") })
	if e := selfupdate.Save(r.file("install.json"), map[string]string{"digest": good}); e != nil {
		t.Fatal(e)
	}
	wait(t, func() bool { s, _ := r.load(); return s.Current == good && s.Phase == "committed" })
	s, _ := r.load()
	if s.Previous != old {
		t.Fatal(s)
	}
	if e := r.Stop(); e != nil {
		t.Fatal(e)
	}
	if r.alive("child.json") {
		t.Fatal("child survived stop")
	}
}

func TestInstallCurrentAfterRollbackIsNoOp(t *testing.T) {
	r, old, bad := fixture(t)
	state := State{Current: old, Previous: old, Target: bad, Phase: "failed", Failed: []string{bad}, Error: "trial failed"}
	if err := r.save(state); err != nil {
		t.Fatal(err)
	}
	if err := r.Install(r.Binary(old)); err != nil {
		t.Fatal(err)
	}
	next, err := r.load()
	if err != nil || next.Phase != "failed" || next.Current != old || next.Target != bad || next.Error != state.Error {
		t.Fatalf("reinstall changed rollback state: %+v, %v", next, err)
	}
	if _, err := os.Stat(r.file("install.json")); !os.IsNotExist(err) {
		t.Fatalf("unexpected update request: %v", err)
	}
}
func TestTrialFailuresRollback(t *testing.T) {
	for _, mode := range []byte{'B', 'H', 'O'} {
		t.Run(string(mode), func(t *testing.T) {
			r, old, _ := fixture(t)
			exe, _ := os.Executable()
			b, _ := os.ReadFile(exe)
			p := filepath.Join(t.TempDir(), "bad")
			os.WriteFile(p, append(b, mode), 0700)
			bad, e := r.stage(p)
			if e != nil {
				t.Fatal(e)
			}
			r.save(State{Current: old, Previous: old, Target: bad, Phase: "pending"})
			run(t, r)
			wait(t, func() bool { s, _ := r.load(); return s.Phase == "failed" && s.Current == old && r.alive("child.json") })
			resolved, _ := filepath.EvalSymlinks(filepath.Join(r.Root, "bin", "deployer-agent"))
			if resolved != r.Binary(old) {
				t.Fatal(resolved)
			}
			s, _ := r.load()
			if len(s.Failed) != 1 || s.Failed[0] != bad {
				t.Fatal(s)
			}
		})
	}
}
func TestInterruptedTrialRollsBackBeforeLaunch(t *testing.T) {
	r, old, good := fixture(t)
	r.save(State{Current: old, Previous: old, Target: good, Phase: "trial"})
	r.link(good)
	run(t, r)
	wait(t, func() bool { s, _ := r.load(); return s.Phase == "failed" && r.alive("child.json") })
	s, _ := r.load()
	if s.Current != old {
		t.Fatal(s)
	}
}
func TestStopWinsPendingUpdate(t *testing.T) {
	r, old, good := fixture(t)
	r.save(State{Current: old, Previous: old, Target: good, Phase: "pending"})
	selfupdate.Atomic(r.file("stop"), []byte("stop"), 0600)
	if e := r.Run(); e != nil {
		t.Fatal(e)
	}
	if r.alive("child.json") {
		t.Fatal("started after stop")
	}
}
func TestIdentityAndEnvironment(t *testing.T) {
	r, _, _ := fixture(t)
	p, e := Identity(os.Getpid())
	if e != nil || !p.Alive() {
		t.Fatal(e)
	}
	p.Start = "wrong"
	if p.Alive() {
		t.Fatal("PID identity ignored")
	}
	if e = p.Signal(syscall.SIGKILL); e != nil {
		t.Fatal(e)
	}
	marker := filepath.Join(r.Root, "must-not-exist")
	os.WriteFile(filepath.Join(r.Root, "config", "agent.env"), []byte(fmt.Sprintf("VALUE=$(touch %s)\n", marker)), 0600)
	env, e := r.Environment()
	if e != nil || len(env) < 1 || Exists(marker) {
		t.Fatal(env, e)
	}
}

func TestInterruptedRemoveIsIdempotent(t *testing.T) {
	r, old, _ := fixture(t)
	if e := r.save(State{Current: old, Phase: "removing"}); e != nil {
		t.Fatal(e)
	}
	// Simulate interruption after binaries were removed but before state deletion.
	if e := os.RemoveAll(filepath.Join(r.Root, "releases", r.Role)); e != nil {
		t.Fatal(e)
	}
	if e := r.Remove(); e != nil {
		t.Fatal(e)
	}
	if e := r.Remove(); e != nil {
		t.Fatal(e)
	}
	if _, e := r.load(); !os.IsNotExist(e) {
		t.Fatal("removal state survived", e)
	}
}
