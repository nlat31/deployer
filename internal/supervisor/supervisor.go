package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"deployer/internal/model"
	"deployer/internal/packages"
	"deployer/internal/store"

	"golang.org/x/sys/unix"
)

type Spec struct {
	Root        string
	AppID       string
	Version     string
	OperationID string
	Env         map[string]string
}

func (s Spec) Dir() string { return filepath.Join(s.Root, "apps", s.AppID, "versions", s.Version) }
func (s Spec) Environment() map[string]string {
	env := map[string]string{"APP_ID": s.AppID, "APP_VERSION": s.Version, "APP_RELEASE_DIR": s.Dir(), "APP_DATA_DIR": filepath.Join(s.Dir(), "data"), "APP_CONFIG_DIR": filepath.Join(s.Dir(), "config"), "APP_LOG_DIR": filepath.Join(s.Dir(), "logs"), "APP_RUN_DIR": filepath.Join(s.Dir(), "run"), "DEPLOY_OPERATION_ID": s.OperationID}
	for k, v := range s.Env {
		env[k] = v
	}
	return env
}

type Status struct {
	Running   bool
	PID       int
	State     string
	StartedAt int64
}
type Interface interface {
	Prepare(Spec) error
	Hook(context.Context, Spec, string, func(string)) error
	Start(context.Context, Spec) error
	Stop(context.Context, Spec, func(string)) error
	Status(context.Context, Spec) (Status, error)
	Logs(context.Context, Spec) (string, error)
}

// Direct executes trusted App scripts as the Agent's root user, without an App
// service-manager unit. start.sh must remain in the foreground (usually exec the App).
type Direct struct{}

type processRecord struct {
	PID       int    `json:"pid"`
	Start     string `json:"start"`
	Token     string `json:"token"`
	Boot      string `json:"boot"`
	StartedAt int64  `json:"started_at"`
}

func (*Direct) Check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("Agent 必须以 root 运行，应用脚本直接使用 root 执行")
	}
	fd, err := unix.PidfdOpen(os.Getpid(), 0)
	if err != nil {
		return fmt.Errorf("Linux pidfd 不可用: %w", err)
	}
	defer unix.Close(fd)
	if err = unix.PidfdSendSignal(fd, 0, nil, 0); err != nil {
		return fmt.Errorf("Linux pidfd 信号不可用: %w", err)
	}
	return nil
}
func (*Direct) Prepare(sp Spec) error {
	if !model.ValidName(sp.AppID) || !model.ValidName(sp.Version) {
		return fmt.Errorf("非法运行目录")
	}
	if err := packages.PlainDir(sp.Dir()); err != nil {
		return err
	}
	for _, name := range []string{"data", "config", "logs", "run"} {
		path := filepath.Join(sp.Dir(), name)
		if err := os.MkdirAll(path, 0700); err != nil {
			return err
		}
		if err := packages.PlainDir(path); err != nil {
			return err
		}
	}
	return nil
}

// StopAll is used only after the Agent has stopped, before uninstall removes
// process records. Failure preserves those records and the application files.
func (s *Direct) StopAll(ctx context.Context, root string) error {
	apps, err := os.ReadDir(filepath.Join(root, "apps"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, app := range apps {
		if !app.IsDir() || !model.ValidName(app.Name()) {
			continue
		}
		versionsPath := filepath.Join(root, "apps", app.Name(), "versions")
		versions, err := os.ReadDir(versionsPath)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		for _, version := range versions {
			if !version.IsDir() || !model.ValidName(version.Name()) {
				continue
			}
			sp := Spec{Root: root, AppID: app.Name(), Version: version.Name(), OperationID: model.ID()}
			if err = packages.PlainDir(sp.Dir()); err != nil {
				return err
			}
			run := filepath.Join(sp.Dir(), "run")
			if _, err = os.Stat(run); os.IsNotExist(err) {
				continue
			}
			if err = packages.PlainDir(run); err != nil {
				return err
			}
			hooks, err := filepath.Glob(filepath.Join(run, "hook-*.json"))
			if err != nil {
				return err
			}
			for _, hook := range hooks {
				if err = stopRecord(ctx, hook); err != nil {
					return err
				}
			}
			if err = s.Stop(ctx, sp, func(string) {}); err != nil {
				return err
			}
		}
	}
	return nil
}

func recordPath(sp Spec, name string) string { return filepath.Join(sp.Dir(), "run", name+".json") }
func procInfo(pid int) (state, start string, group int, err error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", "", 0, err
	}
	end := strings.LastIndexByte(string(b), ')')
	if end < 0 {
		return "", "", 0, fmt.Errorf("损坏的进程信息")
	}
	fields := strings.Fields(string(b)[end+1:])
	if len(fields) < 20 {
		return "", "", 0, fmt.Errorf("损坏的进程信息")
	}
	group, err = strconv.Atoi(fields[2])
	return fields[0], fields[19], group, err
}
func loadRecord(path string) (processRecord, error) {
	var r processRecord
	b, err := os.ReadFile(path)
	if err != nil {
		return r, err
	}
	if err = json.Unmarshal(b, &r); err != nil {
		return r, err
	}
	if r.PID <= 1 || r.Start == "" || r.Token == "" || r.Boot == "" {
		return r, fmt.Errorf("无效的进程记录 %s", path)
	}
	return r, nil
}

// A start-time check prevents signaling a reused PID. If the leader exited,
// verify the inherited random identity on surviving group members instead.
func members(r processRecord) ([]int, error) {
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(string(boot)) != r.Boot {
		return nil, nil
	}
	_, birth, _, leaderErr := procInfo(r.PID)
	if leaderErr == nil && birth != r.Start {
		return nil, nil
	}
	if leaderErr != nil && !os.IsNotExist(leaderErr) {
		return nil, leaderErr
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var result []int
	for _, entry := range entries {
		pid, e := strconv.Atoi(entry.Name())
		if e != nil {
			continue
		}
		state, start, group, e := procInfo(pid)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return nil, e
		}
		if group != r.PID || state == "Z" || state == "X" {
			continue
		}
		if pid == r.PID && start == r.Start {
			result = append(result, pid)
			continue
		}
		env, e := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return nil, e
		}
		if bytes.Contains(append([]byte{0}, env...), []byte("\x00DEPLOYER_PROCESS_ID="+r.Token+"\x00")) {
			result = append(result, pid)
		}
	}
	return result, nil
}
func signalMembers(r processRecord, sig syscall.Signal) error {
	pids, err := members(r)
	if err != nil {
		return err
	}
	for _, pid := range pids {
		fd, e := unix.PidfdOpen(pid, 0)
		if e == unix.ESRCH {
			continue
		}
		if e != nil {
			return fmt.Errorf("pidfd_open: %w", e)
		}
		// Opening a pidfd before validating pins the exact process being signaled.
		current, checkErr := members(r)
		matched := false
		for _, p := range current {
			if p == pid {
				matched = true
			}
		}
		if checkErr == nil && matched {
			e = unix.PidfdSendSignal(fd, unix.Signal(sig), nil, 0)
		} else {
			e = checkErr
		}
		unix.Close(fd)
		if e != nil && e != unix.ESRCH {
			return e
		}
	}
	return nil
}

func stopRecord(ctx context.Context, path string) error {
	r, err := loadRecord(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err = signalMembers(r, syscall.SIGTERM); err != nil {
		return err
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		pids, e := members(r)
		if e != nil {
			return e
		}
		if len(pids) == 0 {
			return os.Remove(path)
		}
		if time.Now().After(deadline) {
			if e = signalMembers(r, syscall.SIGKILL); e != nil {
				return e
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}
func (s *Direct) Quiesce(ctx context.Context, sp Spec) error {
	for _, name := range []string{"setup.sh", "start.sh", "stop.sh", "clean.sh"} {
		if err := stopRecord(ctx, recordPath(sp, "hook-"+name+"-"+sp.OperationID)); err != nil {
			return err
		}
	}
	return nil
}
func command(sp Spec, script string) *exec.Cmd {
	// The pipe gate closes the launch/persist crash window: no script executes
	// until its process identity has been durably saved. EOF aborts the launch.
	cmd := exec.Command("/bin/bash", "-c", `IFS= read -r gate <&3 || exit 125; exec 3<&-; exec /bin/bash "$1"`, "deployer", filepath.Join(sp.Dir(), script))
	cmd.WaitDelay = 2 * time.Second
	cmd.Dir = sp.Dir()
	cmd.Env = os.Environ()
	for k, v := range sp.Environment() {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd
}
func launch(cmd *exec.Cmd, path string) (<-chan error, error) {
	token := model.ID()
	cmd.Env = append(cmd.Env, "DEPLOYER_PROCESS_ID="+token)
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	defer writer.Close()
	cmd.ExtraFiles = []*os.File{reader}
	if err = cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	_, start, _, err := procInfo(cmd.Process.Pid)
	if err == nil {
		boot, bootErr := os.ReadFile("/proc/sys/kernel/random/boot_id")
		if bootErr != nil {
			err = bootErr
		} else {
			b, _ := json.Marshal(processRecord{PID: cmd.Process.Pid, Start: start, Token: token, Boot: strings.TrimSpace(string(boot)), StartedAt: model.Now()})
			err = store.AtomicWrite(path, b, 0600)
		}
	}
	if err == nil {
		_, err = writer.Write([]byte("run\n"))
	}
	if err != nil {
		_ = cmd.Process.Kill()
		<-done
		return nil, err
	}
	return done, nil
}
func (s *Direct) Hook(ctx context.Context, sp Spec, script string, log func(string)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if script != "setup.sh" && script != "start.sh" && script != "stop.sh" && script != "clean.sh" {
		return fmt.Errorf("不允许的脚本")
	}
	limit := 15 * time.Minute
	if script == "stop.sh" || script == "start.sh" {
		limit = 30 * time.Second
	}
	if script == "clean.sh" {
		limit = time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	path := recordPath(sp, "hook-"+script+"-"+sp.OperationID)
	if err := stopRecord(ctx, path); err != nil {
		return err
	}
	cmd := command(sp, script)
	out := &boundedWriter{max: 64 << 10}
	cmd.Stdout = out
	cmd.Stderr = out
	done, err := launch(cmd, path)
	if err != nil {
		return err
	}
	finished := false
	select {
	case err = <-done:
		finished = true
	case <-ctx.Done():
		err = ctx.Err()
	}
	cleanCtx, cancelClean := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelClean()
	stopErr := stopRecord(cleanCtx, path)
	if !finished {
		select {
		case <-done:
		case <-cleanCtx.Done():
		}
	}
	if text := out.String(); text != "" {
		log(text)
	}
	if stopErr != nil {
		return fmt.Errorf("%s 清理失败: %w", script, stopErr)
	}
	if err != nil {
		text := out.String()
		if len(text) > 4096 {
			text = text[len(text)-4096:]
		}
		return fmt.Errorf("%s 执行失败: %w: %s", script, err, strings.TrimSpace(text))
	}
	return nil
}
func (s *Direct) Start(ctx context.Context, sp Spec) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	st, err := s.Status(ctx, sp)
	if err != nil {
		return err
	}
	if err = s.Prepare(sp); err != nil {
		return err
	}
	file, err := os.OpenFile(filepath.Join(sp.Dir(), "logs", "process.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	// Always invoke start.sh on an explicit start. If an App is already live,
	// preserve its process record and let the App script implement idempotence.
	if st.Running {
		env := map[string]string{}
		for key, value := range sp.Env {
			env[key] = value
		}
		env["APP_MAIN_PID"] = strconv.Itoa(st.PID)
		sp.Env = env
		var writeErr error
		err := s.Hook(ctx, sp, "start.sh", func(text string) { _, writeErr = io.WriteString(file, text) })
		if err != nil {
			return err
		}
		return writeErr
	}
	cmd := command(sp, "start.sh")
	cmd.Stdout = file
	cmd.Stderr = file
	_, err = launch(cmd, recordPath(sp, "process"))
	return err
}
func (s *Direct) Stop(ctx context.Context, sp Spec, log func(string)) error {
	st, err := s.Status(ctx, sp)
	if err != nil {
		return err
	}
	if st.Running {
		env := map[string]string{}
		for k, v := range sp.Env {
			env[k] = v
		}
		env["APP_MAIN_PID"] = strconv.Itoa(st.PID)
		sp.Env = env
		if err = s.Hook(ctx, sp, "stop.sh", log); err != nil {
			return err
		}
	}
	cleanCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return stopRecord(cleanCtx, recordPath(sp, "process"))
}
func trimLog(sp Spec) error {
	path := filepath.Join(sp.Dir(), "logs", "process.log")
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Size() <= 16<<20 {
		return nil
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	tail := make([]byte, 64<<10)
	n, err := file.ReadAt(tail, info.Size()-int64(len(tail)))
	if err != nil && err != io.EOF {
		return err
	}
	if err = file.Truncate(0); err != nil {
		return err
	}
	_, err = file.WriteAt(tail[:n], 0)
	return err
}

func (*Direct) Status(ctx context.Context, sp Spec) (Status, error) {
	if err := ctx.Err(); err != nil {
		return Status{}, err
	}
	r, err := loadRecord(recordPath(sp, "process"))
	if errors.Is(err, os.ErrNotExist) {
		return Status{State: "inactive"}, nil
	}
	if err != nil {
		return Status{}, err
	}
	pids, err := members(r)
	if err != nil {
		return Status{}, err
	}
	if err = trimLog(sp); err != nil {
		return Status{}, err
	}
	if len(pids) == 0 {
		return Status{State: "inactive", StartedAt: r.StartedAt}, nil
	}
	// A daemonized child still counts as running so stop cannot miss it.
	pid := pids[0]
	for _, p := range pids {
		if p == r.PID {
			pid = p
		}
	}
	return Status{Running: true, PID: pid, State: "active", StartedAt: r.StartedAt}, nil
}
func (*Direct) Logs(ctx context.Context, sp Spec) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	file, err := os.Open(filepath.Join(sp.Dir(), "logs", "process.log"))
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	if info.Size() > 64<<10 {
		if _, err = file.Seek(-(64 << 10), io.SeekEnd); err != nil {
			return "", err
		}
	}
	b, err := io.ReadAll(io.LimitReader(file, 64<<10))
	return string(b), err
}

type boundedWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
	max int
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(p)
	remain := w.max - w.buf.Len()
	if remain > 0 {
		if len(p) > remain {
			p = p[:remain]
		}
		w.buf.Write(p)
	}
	return n, nil
}

func (w *boundedWriter) String() string { w.mu.Lock(); defer w.mu.Unlock(); return w.buf.String() }

func Probe(ctx context.Context, s Interface, sp Spec, h model.Health) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	st, err := s.Status(ctx, sp)
	if err != nil {
		return err
	}
	if !st.Running || st.PID <= 0 {
		return fmt.Errorf("主进程未运行")
	}
	switch h.Type {
	case "http":
		client := &http.Client{Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		req, err := http.NewRequestWithContext(ctx, "GET", h.Target, nil)
		if err != nil {
			return err
		}
		res, err := client.Do(req)
		if err != nil {
			return err
		}
		defer res.Body.Close()
		if res.StatusCode < 200 || res.StatusCode >= 300 {
			body, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
			return fmt.Errorf("HTTP 状态 %d: %s", res.StatusCode, strings.TrimSpace(string(body)))
		}
	case "tcp":
		c, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "tcp", h.Target)
		if err != nil {
			return err
		}
		c.Close()
	}
	return nil
}

func Healthy(ctx context.Context, s Interface, sp Spec, h model.Health) error {
	var e error
	h, e = h.Normalize()
	if e != nil {
		return e
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(h.TimeoutSeconds)*time.Second)
	defer cancel()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	var since time.Time
	client := &http.Client{Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	last := "等待进程启动"
	for {
		if ctx.Err() != nil {
			return fmt.Errorf("健康检查失败: %s: %w", last, ctx.Err())
		}
		st, e := s.Status(ctx, sp)
		ok := e == nil && st.Running && st.PID > 0
		if !ok {
			if ctx.Err() != nil {
				return fmt.Errorf("健康检查失败: %s: %w", last, ctx.Err())
			}
			if e != nil {
				last = e.Error()
			} else {
				last = "主进程未运行"
			}
		}
		if ok && h.Type == "http" {
			req, _ := http.NewRequestWithContext(ctx, "GET", h.Target, nil)
			resp, err := client.Do(req)
			ok = err == nil
			if resp != nil {
				body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
				resp.Body.Close()
				ok = err == nil && resp.StatusCode >= 200 && resp.StatusCode < 300
				if !ok && ctx.Err() == nil {
					last = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
				}
			} else if err != nil && ctx.Err() == nil {
				last = "HTTP 健康探测失败: " + err.Error()
			}
		}
		if ok && h.Type == "tcp" {
			conn, e := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "tcp", h.Target)
			ok = e == nil
			if conn != nil {
				conn.Close()
			}
			if !ok {
				last = "TCP 健康探测未通过"
			}
		}
		if ok {
			if since.IsZero() {
				since = time.Now()
			}
			if time.Since(since) >= time.Duration(h.StableSeconds)*time.Second {
				return nil
			}
		} else {
			since = time.Time{}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("健康检查失败: %s: %w", last, ctx.Err())
		case <-ticker.C:
		}
	}
}
