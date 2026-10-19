package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"deployer/internal/controller"
	"deployer/web"
)

func main() {
	addr := flag.String("listen", env("DEPLOYER_LISTEN", "127.0.0.1:7710"), "HTTP(S) 监听地址")
	data := flag.String("data", env("DEPLOYER_DATA", "./data/server"), "独立数据目录")
	cert := flag.String("tls-cert", os.Getenv("DEPLOYER_TLS_CERT"), "TLS 证书路径")
	key := flag.String("tls-key", os.Getenv("DEPLOYER_TLS_KEY"), "TLS 私钥路径")
	clientCA := flag.String("client-ca", os.Getenv("DEPLOYER_CLIENT_CA"), "信任的 Agent CA 证书（必需）")
	uiAddr := flag.String("ui-listen", os.Getenv("DEPLOYER_UI_LISTEN"), "可选的回环 HTTP 页面监听地址")
	flag.Parse()
	if _, _, err := net.SplitHostPort(*addr); err != nil {
		log.Fatal(err)
	}
	if *cert == "" || *key == "" || *clientCA == "" {
		log.Fatal("必须设置 --tls-cert、--tls-key 和 --client-ca，Agent 仅支持 mTLS")
	}
	if *uiAddr != "" {
		host, _, err := net.SplitHostPort(*uiAddr)
		if err != nil {
			log.Fatal(err)
		}
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			log.Fatal("--ui-listen 只允许回环地址")
		}
	}
	secure := *uiAddr == ""
	s, err := controller.New(*data, secure, web.Handler())
	if err != nil {
		log.Fatal(err)
	}
	defer s.Close()
	if err = s.ConfigureEnrollment(*clientCA); err != nil {
		log.Fatal(err)
	}
	server := &http.Server{Addr: *addr, Handler: s, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 10 * time.Minute, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
	server.TLSConfig, err = controller.MTLSConfig(*cert, *key, s.ClientCA)
	if err != nil {
		log.Fatal(err)
	}
	var localUI *http.Server
	if *uiAddr != "" {
		// The HTTP listener never exposes Agent APIs; mTLS is mandatory there.
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/agent/") || r.URL.Path == "/enroll" {
				http.Error(w, "mTLS required", http.StatusUnauthorized)
				return
			}
			s.ServeHTTP(w, r)
		})
		localUI = &http.Server{Addr: *uiAddr, Handler: handler, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 10 * time.Minute, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
		go func() {
			log.Printf("本机控制页面 http://%s", *uiAddr)
			if err := localUI.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Fatal(err)
			}
		}()
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
		if localUI != nil {
			_ = localUI.Shutdown(shutdown)
		}
	}()
	log.Printf("Deployer 控制端 %s 数据目录 %s", *addr, s.Root)
	err = server.ListenAndServeTLS("", "")
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
