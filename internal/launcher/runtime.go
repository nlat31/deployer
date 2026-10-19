// Package launcher provides a small root-owned supervisor, independent of Agent state.
package launcher

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"deployer/internal/selfupdate"
	"golang.org/x/sys/unix"
)

type Runtime struct{ Root, Role string }
type Process struct {
	PID   int    `json:"pid"`
	Start string `json:"start"`
	Boot  string `json:"boot"`
}
type Beat struct {
	Token string `json:"token"`
	Phase string `json:"phase"`
	Time  int64  `json:"time"`
}
type State struct {
	Current  string   `json:"current"`
	Previous string   `json:"previous,omitempty"`
	Target   string   `json:"target,omitempty"`
	Phase    string   `json:"phase"`
	Error    string   `json:"error,omitempty"`
	Failed   []string `json:"failed,omitempty"`
}

func (r Runtime) Dir() string          { return filepath.Join(r.Root, "run", r.Role) }
func (r Runtime) file(n string) string { return filepath.Join(r.Dir(), n) }
func (r Runtime) Binary(d string) string {
	return filepath.Join(r.Root, "releases", r.Role, d, "deployer-"+r.Role)
}
func (r Runtime) state() string   { return filepath.Join(r.Root, "updater", r.Role+".json") }
func (r Runtime) request() string { return filepath.Join(r.Root, "updater", r.Role+"-request.json") }
func (r Runtime) Key() string     { return filepath.Join(r.Root, "config", "update-public.key") }
func (r Runtime) Validate() error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("deployer must run as root")
	}
	if r.Role != "agent" && r.Role != "server" {
		return fmt.Errorf("invalid role")
	}
	if !filepath.IsAbs(r.Root) || r.Root == "/" {
		return fmt.Errorf("installation root must be absolute")
	}
	for _, p := range []string{r.Root, filepath.Join(r.Root, "run"), r.Dir(), filepath.Join(r.Root, "bin"), filepath.Join(r.Root, "config"), filepath.Join(r.Root, "releases"), filepath.Join(r.Root, "updater")} {
		info, e := os.Lstat(p)
		if e == nil && (info.Mode()&os.ModeSymlink != 0 || !info.IsDir()) {
			return fmt.Errorf("unsafe installation directory %s", p)
		}
		if e != nil && !os.IsNotExist(e) {
			return e
		}
	}
	return os.MkdirAll(r.Dir(), 0700)
}
func Lock(path string, nonblock bool) (*os.File, error) {
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return nil, e
	}
	f, e := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	flags := syscall.LOCK_EX
	if nonblock {
		flags |= syscall.LOCK_NB
	}
	if e = syscall.Flock(int(f.Fd()), flags); e != nil {
		f.Close()
		return nil, e
	}
	return f, nil
}
func Identity(pid int) (Process, error) {
	b, e := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if e != nil {
		return Process{}, e
	}
	i := strings.LastIndex(string(b), ")")
	if i < 0 {
		return Process{}, fmt.Errorf("bad proc stat")
	}
	fields := strings.Fields(string(b)[i+1:])
	if len(fields) < 20 || fields[0] == "Z" {
		return Process{}, fmt.Errorf("process exited")
	}
	boot, e := os.ReadFile("/proc/sys/kernel/random/boot_id")
	return Process{pid, fields[19], strings.TrimSpace(string(boot))}, e
}
func (p Process) Alive() bool { now, e := Identity(p.PID); return e == nil && p == now }
func (p Process) Signal(s syscall.Signal) error {
	fd, e := unix.PidfdOpen(p.PID, 0)
	if e != nil {
		if !p.Alive() {
			return nil
		}
		return e
	}
	defer unix.Close(fd)
	if !p.Alive() {
		return nil
	}
	return unix.PidfdSendSignal(fd, s, nil, 0)
}
func (r Runtime) alive(name string) bool {
	var p Process
	return selfupdate.Load(r.file(name), &p) == nil && p.Alive()
}
func (r Runtime) load() (State, error) { var s State; e := selfupdate.Load(r.state(), &s); return s, e }
func (r Runtime) save(s State) error   { return selfupdate.Save(r.state(), s) }
func (r Runtime) link(d string) error {
	if !selfupdate.ValidDigest(d) {
		return fmt.Errorf("invalid binary digest")
	}
	if e := os.MkdirAll(filepath.Join(r.Root, "bin"), 0700); e != nil {
		return e
	}
	path := filepath.Join(r.Root, "bin", "deployer-"+r.Role)
	tmp := path + ".next"
	_ = os.Remove(tmp)
	if e := os.Symlink(r.Binary(d), tmp); e != nil {
		return e
	}
	if e := os.Rename(tmp, path); e != nil {
		return e
	}
	return selfupdate.SyncDir(filepath.Dir(path))
}
func (r Runtime) stage(source string) (string, error) {
	d, _, e := selfupdate.Digest(source)
	if e != nil {
		return "", e
	}
	b, e := os.ReadFile(source)
	if e != nil {
		return "", e
	}
	if e = selfupdate.Atomic(r.Binary(d), b, 0700); e != nil {
		return "", e
	}
	actual, _, e := selfupdate.Digest(r.Binary(d))
	if e == nil && actual != d {
		e = fmt.Errorf("source changed during install")
	}
	return d, e
}

// Install is called under the lifecycle lock. A live deployment uses the same
// rollback transaction as an automatic update; configuration is never replaced.
func (r Runtime) Install(source string) error {
	d, e := r.stage(source)
	if e != nil {
		return e
	}
	s, e := r.load()
	if os.IsNotExist(e) {
		s = State{Current: d, Phase: "committed"}
		if e = r.save(s); e != nil {
			return e
		}
		return r.link(d)
	}
	if e != nil {
		return e
	}
	if s.Phase == "removing" {
		return fmt.Errorf("finish remove.sh before reinstalling")
	}
	if s.Current == d && s.Phase == "committed" {
		return r.link(d)
	}
	if r.alive("launcher.json") {
		if e = selfupdate.Save(r.file("install.json"), map[string]string{"digest": d}); e != nil {
			return e
		}
		deadline := time.Now().Add(14 * time.Minute)
		for time.Now().Before(deadline) {
			next, e := r.load()
			if e != nil {
				return e
			}
			if next.Current == d && next.Phase == "committed" {
				return nil
			}
			if next.Target == d && next.Phase == "failed" {
				return fmt.Errorf("install rolled back: %s", next.Error)
			}
			time.Sleep(time.Second)
		}
		return fmt.Errorf("install pending; inspect %s", r.state())
	}
	if r.alive("child.json") {
		return fmt.Errorf("orphan service still running; run stop.sh first")
	}
	// A stopped install is validated on the next start, with the old binary retained.
	s.Previous = s.Current
	s.Target = d
	s.Phase = "pending"
	s.Error = ""
	return r.save(s)
}
func (r Runtime) Start() error {
	if r.alive("launcher.json") {
		var b Beat
		s, e := r.load()
		if e == nil && s.Phase != "trial" && s.Phase != "pending" && r.alive("child.json") && selfupdate.Load(r.file("ready.json"), &b) == nil && b.Phase == "ready" && b.Time > time.Now().Add(-120*time.Second).UnixMilli() {
			return nil
		}
		return fmt.Errorf("launcher is recovering or draining; inspect status/logs")
	}
	if r.alive("child.json") {
		return fmt.Errorf("orphan service detected; stop before starting")
	}
	if s, e := r.load(); e != nil {
		return e
	} else if s.Phase == "removing" {
		return fmt.Errorf("removal in progress; run remove.sh")
	}
	_ = os.Remove(r.file("stop"))
	_ = os.Remove(r.file("drain"))
	exe, e := os.Executable()
	if e != nil {
		return e
	}
	cmd := exec.Command(exe, "run", "--root", r.Root, "--role", r.Role)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if e = cmd.Start(); e != nil {
		return e
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	deadline := time.Now().Add(190 * time.Second)
	for time.Now().Before(deadline) {
		var b Beat
		state, _ := r.load()
		if state.Phase != "trial" && state.Phase != "pending" && selfupdate.Load(r.file("ready.json"), &b) == nil && b.Phase == "ready" && b.Time > time.Now().Add(-10*time.Second).UnixMilli() && r.alive("child.json") {
			return nil
		}
		select {
		case e := <-done:
			return fmt.Errorf("launcher exited: %v", e)
		default:
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("startup not ready; inspect %s", filepath.Join(r.Root, "logs", r.Role+".log"))
}
func (r Runtime) Stop() error {
	if e := selfupdate.Atomic(r.file("stop"), []byte("stop"), 0600); e != nil {
		return e
	}
	if !r.alive("launcher.json") && !r.alive("child.json") {
		return nil
	}
	if e := selfupdate.Atomic(r.file("drain"), []byte("stop"), 0600); e != nil {
		return e
	}
	deadline := time.Now().Add(11 * time.Minute)
	for time.Now().Before(deadline) {
		if !r.alive("launcher.json") {
			var p Process
			if selfupdate.Load(r.file("child.json"), &p) != nil || !p.Alive() {
				return nil
			}
			var b Beat
			if r.Role == "server" || (selfupdate.Load(r.file("ready.json"), &b) == nil && b.Phase == "drained" && b.Time > time.Now().Add(-120*time.Second).UnixMilli()) {
				if e := p.Signal(syscall.SIGTERM); e != nil {
					return e
				}
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("safe stop timed out; stop intent retained, no process was forcibly killed")
}
func (r Runtime) Remove() error {
	if e := r.Stop(); e != nil {
		return e
	}
	s, e := r.load()
	if os.IsNotExist(e) {
		return nil
	}
	if e != nil {
		return e
	}
	if r.Role == "agent" && s.Phase != "removing" {
		env, e := r.Environment()
		if e != nil {
			return e
		}
		cmd := exec.Command(r.Binary(s.Current), "--stop-apps")
		cmd.Env = env
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if e = cmd.Run(); e != nil {
			return fmt.Errorf("stop managed applications: %w", e)
		}
	}
	s.Phase = "removing"
	if e = r.save(s); e != nil {
		return e
	}
	for _, path := range []string{filepath.Join(r.Root, "bin", "deployer-"+r.Role), r.request(), filepath.Join(r.Root, "updater", r.Role+"-download.json"), r.file("install.json"), r.file("trial"), r.file("ready.json")} {
		if e = os.Remove(path); e != nil && !os.IsNotExist(e) {
			return e
		}
	}
	if e = os.RemoveAll(filepath.Join(r.Root, "releases", r.Role)); e != nil {
		return e
	}
	if r.Role == "server" {
		if e = os.Remove(filepath.Join(r.Root, "bin", "deployerctl")); e != nil && !os.IsNotExist(e) {
			return e
		}
	}
	if e = os.RemoveAll(filepath.Join(r.Root, "lifecycle", r.Role)); e != nil {
		return e
	}
	return os.Remove(r.state())
}

// Environment files are literal KEY=value, never executed as shell code.
func (r Runtime) Environment() ([]string, error) {
	f, e := os.Open(filepath.Join(r.Root, "config", r.Role+".env"))
	if e != nil {
		return nil, e
	}
	defer f.Close()
	env := map[string]string{"PATH": "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "HOME": "/root"}
	scan := bufio.NewScanner(f)
	for scan.Scan() {
		s := strings.TrimSpace(scan.Text())
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		k, v, ok := strings.Cut(s, "=")
		if !ok || k == "" || strings.ContainsAny(k, " \t") {
			return nil, fmt.Errorf("invalid environment line")
		}
		env[k] = v
	}
	if e = scan.Err(); e != nil {
		return nil, e
	}
	out := []string{}
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out, nil
}
func Exists(path string) bool { _, e := os.Stat(path); return e == nil }
func Pulse(phase string) error {
	dir := os.Getenv("DEPLOYER_RUNTIME_DIR")
	if dir == "" {
		return nil
	}
	return selfupdate.Save(filepath.Join(dir, "ready.json"), Beat{os.Getenv("DEPLOYER_RUN_TOKEN"), phase, time.Now().UnixMilli()})
}
func Draining() bool {
	dir := os.Getenv("DEPLOYER_RUNTIME_DIR")
	return dir != "" && Exists(filepath.Join(dir, "drain"))
}
func Trial() bool {
	dir := os.Getenv("DEPLOYER_RUNTIME_DIR")
	return dir != "" && Exists(filepath.Join(dir, "trial"))
}
func (r Runtime) Status() (State, error) { return r.load() }

// Bootstrap replaces the launcher only while both roles are stopped.
func (r Runtime) Bootstrap(source string) error {
	target := filepath.Join(r.Root, "bin", "deployer-launcher")
	d, _, e := selfupdate.Digest(source)
	if e != nil {
		return e
	}
	old, _, _ := selfupdate.Digest(target)
	if old == d {
		return nil
	}
	for _, role := range []string{"agent", "server"} {
		other := Runtime{Root: r.Root, Role: role}
		if other.alive("launcher.json") || other.alive("child.json") {
			return fmt.Errorf("launcher changed; stop both roles before setup")
		}
	}
	b, e := os.ReadFile(source)
	if e != nil {
		return e
	}
	return selfupdate.Atomic(target, b, 0700)
}
