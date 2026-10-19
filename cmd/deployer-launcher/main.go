package main

import (
	"deployer/internal/launcher"
	"deployer/internal/selfupdate"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"syscall"
)

func main() {
	if len(os.Args) < 2 {
		log.Fatal("usage: deployer-launcher start|stop|run|install|remove|status --role agent|server --root PATH [--source BINARY]")
	}
	f := flag.NewFlagSet("launcher", flag.ExitOnError)
	root := f.String("root", "/data/deployer", "installation root")
	role := f.String("role", "agent", "role")
	source := f.String("source", "", "install binary")
	lockFD := f.Int("lock-fd", -1, "inherited setup lock descriptor")
	f.Parse(os.Args[2:])
	r := launcher.Runtime{Root: filepath.Clean(*root), Role: *role}
	if e := r.Validate(); e != nil {
		log.Fatal(e)
	}
	if os.Args[1] != "run" {
		path := filepath.Join(r.Root, ".lifecycle.lock")
		if *lockFD >= 0 {
			inherited := os.NewFile(uintptr(*lockFD), "setup-lock")
			stat, e := inherited.Stat()
			expected, other := os.Stat(path)
			if e != nil || other != nil || !os.SameFile(stat, expected) {
				log.Fatal("invalid inherited lifecycle lock")
			}
			if e = syscall.Flock(*lockFD, syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
				log.Fatal(e)
			}
			defer inherited.Close()
		} else {
			lock, e := launcher.Lock(path, false)
			if e != nil {
				log.Fatal(e)
			}
			defer lock.Close()
		}
	}

	var e error
	switch os.Args[1] {
	case "bootstrap":
		e = r.Bootstrap(*source)
	case "run":
		e = r.Run()
	case "start":
		e = r.Start()
	case "stop":
		e = r.Stop()
	case "install":
		e = r.Install(*source)
	case "remove":
		e = r.Remove()
	case "status":
		var s launcher.State
		s, e = r.Status()
		if e == nil {
			fmt.Printf("%+v\n", s)
		}
	case "version":
		fmt.Println(selfupdate.Version)
	default:
		e = fmt.Errorf("unknown command")
	}
	if e != nil {
		log.Fatal(e)
	}
}
