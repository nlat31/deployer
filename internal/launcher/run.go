package launcher

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"deployer/internal/selfupdate"
)

var stablePeriod = 60 * time.Second
var trialTimeout = 180 * time.Second
var drainTimeout = 10 * time.Minute
var candidateDelay = 5 * time.Second
var restartDelay = time.Second

type boundedLog struct {
	mu   sync.Mutex
	path string
}

func (l *boundedLog) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if st, e := os.Stat(l.path); e == nil && st.Size()+int64(len(b)) > 16<<20 {
		if e = os.Rename(l.path, l.path+".1"); e != nil {
			return 0, e
		}
	}
	f, e := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if e != nil {
		return 0, e
	}
	defer f.Close()
	return f.Write(b)
}
func (r Runtime) rollback(s *State, reason string) error {
	s.Phase = "failed"
	s.Error = reason
	s.Failed = append(s.Failed, s.Target)
	s.Current = s.Previous
	if e := r.save(*s); e != nil {
		return e
	}
	return r.link(s.Current)
}
func (r Runtime) candidate(s State) (string, error) {
	var manual map[string]string
	if e := selfupdate.Load(r.file("install.json"), &manual); e == nil {
		d := manual["digest"]
		if !selfupdate.ValidDigest(d) {
			return "", fmt.Errorf("invalid install digest")
		}
		actual, _, e := selfupdate.Digest(r.Binary(d))
		if e != nil || actual != d {
			return "", fmt.Errorf("invalid staged install")
		}
		return d, nil
	}
	if r.Role != "agent" {
		return "", nil
	}
	var m selfupdate.Manifest
	if e := selfupdate.Load(r.request(), &m); os.IsNotExist(e) {
		return "", nil
	} else if e != nil {
		return "", e
	}
	if m.Digest == s.Current {
		return "", nil
	}
	for _, d := range s.Failed {
		if d == m.Digest {
			return "", nil
		}
	}
	key, e := selfupdate.ReadKey(r.Key())
	if e != nil {
		return "", e
	}
	if e = m.Verify(key); e != nil {
		return "", e
	}
	if e = m.Platform(); e != nil {
		return "", e
	}
	if e = m.CheckFile(r.Binary(m.Digest)); e != nil {
		return "", e
	}
	return m.Digest, nil
}
func (r Runtime) Run() error {
	lock, e := Lock(r.file("launcher.lock"), true)
	if e != nil {
		return e
	}
	defer lock.Close()
	if r.alive("child.json") {
		return fmt.Errorf("orphan child still running; run stop.sh")
	}
	p, e := Identity(os.Getpid())
	if e != nil {
		return e
	}
	if e = selfupdate.Save(r.file("launcher.json"), p); e != nil {
		return e
	}
	defer os.Remove(r.file("launcher.json"))
	if e = os.MkdirAll(filepath.Join(r.Root, "logs"), 0700); e != nil {
		return e
	}
	output := &boundedLog{path: filepath.Join(r.Root, "logs", r.Role+".log")}
	logger := log.New(output, "launcher: ", log.LstdFlags)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sig)
	s, e := r.load()
	if e != nil {
		return e
	}
	if s.Phase == "removing" {
		return fmt.Errorf("removal in progress; run remove.sh")
	}
	// An interrupted trial is never assumed healthy. Retain the original rollback target.
	if s.Phase == "trial" {
		if e = r.rollback(&s, "launcher interrupted during trial"); e != nil {
			return e
		}
	}
	if s.Phase == "failed" {
		if e = r.link(s.Current); e != nil {
			return e
		}
	}
	backoff := restartDelay
	for {
		if Exists(r.file("stop")) {
			return nil
		}
		trial := s.Phase == "pending"
		digest := s.Current
		if trial {
			digest = s.Target
			s.Phase = "trial"
			if e = r.save(s); e != nil {
				return e
			}
			if e = r.link(digest); e != nil {
				return e
			}
		}
		_ = os.Remove(r.file("drain"))
		_ = os.Remove(r.file("ready.json"))
		if trial {
			if e = selfupdate.Atomic(r.file("trial"), []byte("trial"), 0600); e != nil {
				return e
			}
		} else {
			_ = os.Remove(r.file("trial"))
		}
		env, e := r.Environment()
		if e != nil {
			return e
		}
		token := fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
		env = append(env, "DEPLOYER_RUNTIME_DIR="+r.Dir(), "DEPLOYER_INSTALL_ROOT="+r.Root, "DEPLOYER_RUN_TOKEN="+token)
		reader, writer, pipeErr := os.Pipe()
		if pipeErr != nil {
			return pipeErr
		}
		// The child cannot execute service code until its identity is durable.
		cmd := exec.Command("/bin/bash", "-c", `IFS= read -r gate <&3 || exit 125; exec 3<&-; exec "$1"`, "deployer-launch", r.Binary(digest))
		cmd.ExtraFiles = []*os.File{reader}
		cmd.Env = env
		cmd.Dir = r.Root
		cmd.Stdout = output
		cmd.Stderr = output
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if e = cmd.Start(); e != nil {
			reader.Close()
			writer.Close()
			if trial {
				if e = r.rollback(&s, e.Error()); e != nil {
					return e
				}
				continue
			}
			return e
		}
		reader.Close()
		child, e := Identity(cmd.Process.Pid)
		if e != nil {
			writer.Close()
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return e
		}
		if e = selfupdate.Save(r.file("child.json"), child); e != nil {
			writer.Close()
			_ = child.Signal(syscall.SIGTERM)
			_ = cmd.Wait()
			return e
		}
		if _, e = writer.Write([]byte("go\n")); e != nil {
			writer.Close()
			_ = child.Signal(syscall.SIGKILL)
			_ = cmd.Wait()
			return e
		}
		writer.Close()
		exited := make(chan error, 1)
		go func() { exited <- cmd.Wait() }()
		began := time.Now()
		healthy := time.Time{}
		drainAt := time.Time{}
		target := ""
		terminating := false
		ticker := time.NewTicker(250 * time.Millisecond)
		restart := false
		for !restart {
			select {
			case <-sig:
				if e = selfupdate.Atomic(r.file("stop"), []byte("stop"), 0600); e != nil {
					logger.Print(e)
				}
			case err := <-exited:
				logger.Printf("child exited: %v", err)
				_ = os.Remove(r.file("child.json"))
				_ = os.Remove(r.file("ready.json"))
				if trial {
					if e = r.rollback(&s, fmt.Sprintf("trial exited: %v", err)); e != nil {
						ticker.Stop()
						return e
					}
				}
				if Exists(r.file("stop")) {
					ticker.Stop()
					return nil
				}
				if target != "" && !trial && terminating {
					s.Previous = s.Current
					s.Target = target
					s.Phase = "pending"
					s.Error = ""
					if e = r.save(s); e != nil {
						ticker.Stop()
						return e
					}
					_ = os.Remove(r.file("install.json"))
				}
				restart = true
			case <-ticker.C:
				var beat Beat
				valid := selfupdate.Load(r.file("ready.json"), &beat) == nil && beat.Token == token && beat.Time > time.Now().Add(-120*time.Second).UnixMilli()
				if trial && !Exists(r.file("stop")) {
					if valid && beat.Phase == "ready" {
						if healthy.IsZero() {
							healthy = time.Now()
						}
						if time.Since(healthy) >= stablePeriod && beat.Time >= healthy.Add(stablePeriod).UnixMilli() {
							s.Current = s.Target
							s.Phase = "committed"
							s.Error = ""
							if e = r.save(s); e != nil {
								ticker.Stop()
								return e
							}
							trial = false
							_ = os.Remove(r.file("trial"))
							_ = os.Remove(r.file("install.json"))
							_ = os.Remove(r.request())
							logger.Printf("committed %s", s.Current)
						}
					} else {
						healthy = time.Time{}
					}
					if trial && time.Since(began) > trialTimeout {
						logger.Print("trial readiness timeout")
						_ = child.Signal(syscall.SIGKILL)
					}
				}
				stopping := Exists(r.file("stop"))
				if !trial && !stopping && target == "" && time.Since(began) > candidateDelay {
					d, err := r.candidate(s)
					if err != nil {
						logger.Print(err)
					} else if d != "" && d != s.Current {
						target = d
					}
				}
				if stopping || target != "" {
					if drainAt.IsZero() {
						if e = selfupdate.Atomic(r.file("drain"), []byte("drain"), 0600); e != nil {
							logger.Print(e)
							continue
						}
						drainAt = time.Now()
					}
					if !terminating && (r.Role == "server" || (valid && beat.Phase == "drained")) {
						if e = child.Signal(syscall.SIGTERM); e != nil {
							logger.Print(e)
						} else {
							terminating = true
						}
					}
					if !stopping && !terminating && time.Since(drainAt) > drainTimeout {
						s.Target = target
						s.Phase = "failed"
						s.Error = "drain timeout; old service kept running"
						s.Failed = append(s.Failed, target)
						if e = r.save(s); e != nil {
							ticker.Stop()
							return e
						}
						target = ""
						drainAt = time.Time{}
						_ = os.Remove(r.file("drain"))
						_ = os.Remove(r.file("install.json"))
					}
				}
			}
		}
		ticker.Stop()
		if time.Since(began) > 60*time.Second {
			backoff = restartDelay
		}
		timer := time.NewTimer(backoff)
		select {
		case <-timer.C:
		case <-sig:
			timer.Stop()
			return selfupdate.Atomic(r.file("stop"), []byte("stop"), 0600)
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

var _ io.Writer = (*boundedLog)(nil)
