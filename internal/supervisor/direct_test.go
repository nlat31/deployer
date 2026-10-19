package supervisor

import (
	"context"
	"deployer/internal/model"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

var discardLog = func(string) {}

func fixture(t *testing.T) (*Direct, Spec) {
	t.Helper()
	sp := Spec{Root: t.TempDir(), AppID: "sample", Version: "1", OperationID: model.ID()}
	if err := os.MkdirAll(sp.Dir(), 0700); err != nil {
		t.Fatal(err)
	}
	s := &Direct{}
	if err := s.Prepare(sp); err != nil {
		t.Fatal(err)
	}
	for name, script := range map[string]string{
		"setup.sh": "id -u > \"$APP_DATA_DIR/uid\"\necho seed > \"$APP_DATA_DIR/seed\"\n",
		"start.sh": `echo call >> "$APP_DATA_DIR/start-calls"
if [[ -n "${APP_MAIN_PID:-}" ]] && kill -0 "$APP_MAIN_PID" 2>/dev/null; then exit 0; fi
echo started
exec sleep 300
`,
		"stop.sh":  "kill -TERM \"$APP_MAIN_PID\" 2>/dev/null || true\n",
		"clean.sh": "find \"$APP_DATA_DIR\" -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +\n",
	} {
		if err := os.WriteFile(filepath.Join(sp.Dir(), name), []byte("#!/bin/bash\nset -eu\n"+script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		if err := s.Stop(context.Background(), sp, discardLog); err != nil {
			t.Error(err)
		}
	})
	return s, sp
}
func waitRunning(t *testing.T, s *Direct, sp Spec, want bool) Status {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		st, err := s.Status(context.Background(), sp)
		if err != nil {
			t.Fatal(err)
		}
		if st.Running == want {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("running=%v want=%v", st.Running, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
func TestDirectLifecycleRecoveryAndClean(t *testing.T) {
	s, sp := fixture(t)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := s.Hook(ctx, sp, "setup.sh", discardLog); err != nil {
			t.Fatal(err)
		}
	}
	uid, _ := os.ReadFile(filepath.Join(sp.Dir(), "data", "uid"))
	if strings.TrimSpace(string(uid)) != fmt.Sprint(os.Geteuid()) {
		t.Fatal("script credentials changed")
	}
	if err := s.Start(ctx, sp); err != nil {
		t.Fatal(err)
	}
	first := waitRunning(t, s, sp, true)
	if err := s.Start(ctx, sp); err != nil {
		t.Fatal(err)
	}
	again := waitRunning(t, &Direct{}, sp, true)
	if first.PID != again.PID {
		t.Fatal("duplicate start")
	}
	if err := Healthy(ctx, s, sp, model.Health{Type: "process", TimeoutSeconds: 3, StableSeconds: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.Stop(ctx, sp, discardLog); err != nil {
		t.Fatal(err)
	}
	waitRunning(t, s, sp, false)
	if err := s.Hook(ctx, sp, "clean.sh", discardLog); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(filepath.Join(sp.Dir(), "data"))
	if len(entries) != 0 {
		t.Fatal("clean did not remove data")
	}
	if err := s.Start(ctx, sp); err != nil {
		t.Fatal(err)
	}
	st := waitRunning(t, s, sp, true)
	if err := syscall.Kill(st.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitRunning(t, s, sp, false)
	if err := s.Start(ctx, sp); err != nil {
		t.Fatal(err)
	}
	waitRunning(t, s, sp, true)
	logs, err := s.Logs(ctx, sp)
	if err != nil || !strings.Contains(logs, "started") {
		t.Fatal(logs, err)
	}
}
func TestDirectStopRemainingChildren(t *testing.T) {
	s, sp := fixture(t)
	os.WriteFile(filepath.Join(sp.Dir(), "start.sh"), []byte("sleep 300 &\nwait\n"), 0700)
	if err := s.Start(context.Background(), sp); err != nil {
		t.Fatal(err)
	}
	st := waitRunning(t, s, sp, true)
	r, err := loadRecord(recordPath(sp, "process"))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		pids, _ := members(r)
		if len(pids) > 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child missing")
		}
		time.Sleep(10 * time.Millisecond)
	}
	syscall.Kill(st.PID, syscall.SIGKILL)
	time.Sleep(30 * time.Millisecond)
	waitRunning(t, &Direct{}, sp, true)
	if err := s.Stop(context.Background(), sp, discardLog); err != nil {
		t.Fatal(err)
	}
	waitRunning(t, s, sp, false)
}
func TestDirectDoesNotSignalReusedPID(t *testing.T) {
	s, sp := fixture(t)
	_, start, _, err := procInfo(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	boot, _ := os.ReadFile("/proc/sys/kernel/random/boot_id")
	b, _ := json.Marshal(processRecord{PID: os.Getpid(), Start: start + "0", Token: model.ID(), Boot: strings.TrimSpace(string(boot))})
	os.WriteFile(recordPath(sp, "process"), b, 0600)
	st, err := s.Status(context.Background(), sp)
	if err != nil || st.Running {
		t.Fatal(st, err)
	}
	if err = s.Stop(context.Background(), sp, discardLog); err != nil {
		t.Fatal(err)
	}
}
func TestDirectInterruptedHookQuiesces(t *testing.T) {
	_, sp := fixture(t)
	os.WriteFile(filepath.Join(sp.Dir(), "setup.sh"), []byte("sleep 300 &\nwait\n"), 0700)
	cmd := command(sp, "setup.sh")
	done, err := launch(cmd, recordPath(sp, "hook-setup.sh-"+sp.OperationID))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(40 * time.Millisecond)
	if err = (&Direct{}).Quiesce(context.Background(), sp); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("hook not reaped")
	}
}
func TestDirectHookCancellation(t *testing.T) {
	s, sp := fixture(t)
	os.WriteFile(filepath.Join(sp.Dir(), "setup.sh"), []byte("sleep 300 &\nwait\n"), 0700)
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	if err := s.Hook(ctx, sp, "setup.sh", discardLog); err == nil {
		t.Fatal("expected cancellation")
	}
	if _, err := os.Stat(recordPath(sp, "hook-setup.sh-"+sp.OperationID)); !os.IsNotExist(err) {
		t.Fatal("hook record not cleaned", err)
	}
}
func TestHealthFailurePreservesResponse(t *testing.T) {
	s, sp := fixture(t)
	if err := s.Start(context.Background(), sp); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		fmt.Fprint(w, `{"error":"No usable sandbox!"}`)
	}))
	defer server.Close()
	err := Healthy(context.Background(), s, sp, model.Health{Type: "http", Target: server.URL, TimeoutSeconds: 1, StableSeconds: 1})
	if err == nil || !strings.Contains(err.Error(), "503") || !strings.Contains(err.Error(), "No usable sandbox!") {
		t.Fatalf("lost diagnostics: %v", err)
	}
}

func TestDirectStopAllBeforeUninstall(t *testing.T) {
	s, sp := fixture(t)
	if err := s.Start(context.Background(), sp); err != nil {
		t.Fatal(err)
	}
	waitRunning(t, s, sp, true)
	if err := s.StopAll(context.Background(), sp.Root); err != nil {
		t.Fatal(err)
	}
	waitRunning(t, s, sp, false)
}
func TestDirectCheckRequiresRoot(t *testing.T) {
	err := (&Direct{}).Check(context.Background())
	if (err == nil) != (os.Geteuid() == 0) {
		t.Fatal("root requirement", err)
	}
}

func TestDirectLogLimit(t *testing.T) {
	s, sp := fixture(t)
	file, err := os.Create(filepath.Join(sp.Dir(), "logs", "process.log"))
	if err != nil {
		t.Fatal(err)
	}
	if err = file.Truncate(17 << 20); err != nil {
		t.Fatal(err)
	}
	if _, err = file.WriteAt([]byte("recent diagnostic"), (17<<20)-17); err != nil {
		t.Fatal(err)
	}
	file.Close()
	if err = trimLog(sp); err != nil {
		t.Fatal(err)
	}
	text, err := s.Logs(context.Background(), sp)
	if err != nil || !strings.Contains(text, "recent diagnostic") {
		t.Fatal("tail missing", err)
	}
	info, _ := os.Stat(filepath.Join(sp.Dir(), "logs", "process.log"))
	if info.Size() > 64<<10 {
		t.Fatal("unbounded log")
	}
}

func TestDirectRejectsPreviousBootIdentity(t *testing.T) {
	s, sp := fixture(t)
	if err := s.Start(context.Background(), sp); err != nil {
		t.Fatal(err)
	}
	path := recordPath(sp, "process")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer os.WriteFile(path, original, 0600)
	record, err := loadRecord(path)
	if err != nil {
		t.Fatal(err)
	}
	record.Boot = "previous-boot"
	data, _ := json.Marshal(record)
	os.WriteFile(path, data, 0600)
	st, err := s.Status(context.Background(), sp)
	if err != nil || st.Running {
		t.Fatal("stale boot accepted", st, err)
	}
}

func TestExplicitStartAlwaysExecutesApplicationScript(t *testing.T) {
	s, sp := fixture(t)
	began := model.Now()
	if err := s.Start(context.Background(), sp); err != nil {
		t.Fatal(err)
	}
	first := waitRunning(t, s, sp, true)
	if first.StartedAt < began || first.StartedAt > model.Now() {
		t.Fatal("missing actual process start time", first)
	}
	if err := s.Start(context.Background(), sp); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(sp.Dir(), "data/start-calls"))
	if err != nil || strings.Count(string(body), "call") != 2 {
		t.Fatal("start script was skipped", string(body), err)
	}
	if next := waitRunning(t, &Direct{}, sp, true); next.PID != first.PID || next.StartedAt != first.StartedAt {
		t.Fatal("App idempotence lost", next)
	}
	if err := os.WriteFile(filepath.Join(sp.Dir(), "start.sh"), []byte("#!/bin/bash\necho repeated-start-error >&2\nexit 17\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background(), sp); err == nil || !strings.Contains(err.Error(), "17") {
		t.Fatal("script error was not reported", err)
	}
	if next := waitRunning(t, s, sp, true); next.PID != first.PID {
		t.Fatal("script failure stopped original App", next)
	}
	logs, err := s.Logs(context.Background(), sp)
	if err != nil || !strings.Contains(logs, "repeated-start-error") {
		t.Fatal("script stderr was lost", logs, err)
	}
}

func TestDirectStopScriptFailureIsReportedWithoutKillingApp(t *testing.T) {
	s, sp := fixture(t)
	if err := s.Start(context.Background(), sp); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(sp.Dir(), "stop.sh")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer os.WriteFile(path, original, 0755)
	if err = os.WriteFile(path, []byte("#!/bin/bash\necho intentional-stop-error >&2\nexit 42\n"), 0755); err != nil {
		t.Fatal(err)
	}
	err = s.Stop(context.Background(), sp, discardLog)
	if err == nil || !strings.Contains(err.Error(), "intentional-stop-error") || !strings.Contains(err.Error(), "42") {
		t.Fatal("script failure lost", err)
	}
	st, err := s.Status(context.Background(), sp)
	if err != nil || !st.Running {
		t.Fatal("failed stop killed app", st, err)
	}
	if err = os.WriteFile(path, original, 0755); err != nil {
		t.Fatal(err)
	}
	if err = s.Stop(context.Background(), sp, discardLog); err != nil {
		t.Fatal(err)
	}
}
