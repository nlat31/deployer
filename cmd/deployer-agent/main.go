package main

import (
	"context"
	"deployer/internal/selfupdate"
	"encoding/json"
	"flag"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"deployer/internal/agent"
	"deployer/internal/identity"
	"deployer/internal/supervisor"
)

func main() {
	version := flag.Bool("version", false, "print build metadata")
	hostname, _ := os.Hostname()
	server := flag.String("server", env("DEPLOYER_SERVER", "https://18.138.124.233:7710"), "控制端根地址")
	data := flag.String("data", env("DEPLOYER_DATA", "./data/agent"), "独立数据目录")
	ca := flag.String("ca", os.Getenv("DEPLOYER_CA"), "信任的服务端 CA 证书（必需）")
	cert := flag.String("cert", os.Getenv("DEPLOYER_CLIENT_CERT"), "客户端证书（必需）")
	key := flag.String("key", os.Getenv("DEPLOYER_CLIENT_KEY"), "客户端私钥（必需）")
	name := flag.String("name", env("DEPLOYER_NODE_NAME", hostname), "节点名称")
	stopApps := flag.Bool("stop-apps", false, "Agent 已停止时，停止此数据目录下的全部应用（用于卸载）")
	flag.Parse()
	if *version {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]string{"version": selfupdate.Version, "commit": selfupdate.Commit, "os": runtime.GOOS, "arch": runtime.GOARCH, "compatibility": selfupdate.Compatibility})
		return
	}
	if os.Geteuid() != 0 {
		log.Fatal("deployer must run as root")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	sup := &supervisor.Direct{}
	check, cancel := context.WithTimeout(ctx, 10*time.Second)
	err := sup.Check(check)
	cancel()
	if err != nil {
		log.Fatal(err)
	}
	if *stopApps {
		stateDir := filepath.Join(*data, "state")
		if err = os.MkdirAll(stateDir, 0700); err != nil {
			log.Fatal(err)
		}
		lock, openErr := os.OpenFile(filepath.Join(stateDir, "agent.db.lock"), os.O_CREATE|os.O_RDWR, 0600)
		if openErr != nil {
			log.Fatal(openErr)
		}
		defer lock.Close()
		if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			log.Fatal("Agent 仍在运行，无法停止全部应用: ", err)
		}
		defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		if err = sup.StopAll(ctx, *data); err != nil {
			log.Fatal(err)
		}
		return
	}
	if err = identity.EnsureIdentity(ctx, *server, *ca, *cert, *key); err != nil {
		log.Fatal(err)
	}
	go func() {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if e := identity.EnsureIdentity(ctx, *server, *ca, *cert, *key); e != nil {
					log.Printf("证书续期: %v", e)
				}
			}
		}
	}()
	c, err := agent.Open(*data, *server, agent.TLSFiles{CA: *ca, Cert: *cert, Key: *key}, sup)
	if err != nil {
		log.Fatal(err)
	}
	defer c.Close()
	if *name != "" {
		c.Name = *name
	}
	log.Printf("Agent %s，数据目录 %s", c.Identity.NodeID, c.Root)
	if err = c.Run(ctx); err != nil {
		log.Fatal(err)
	}
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
