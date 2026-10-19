package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8877"
	}
	message := os.Getenv("MESSAGE")
	if message == "" {
		message = "Hello from Deployer"
	}
	if os.Getenv("FAIL_START") == "1" {
		log.Fatal("intentional startup failure")
	}
	data := os.Getenv("APP_DATA_DIR")
	if data == "" {
		log.Fatal("APP_DATA_DIR is required")
	}
	if e := os.WriteFile(filepath.Join(data, "last-start.txt"), []byte(time.Now().Format(time.RFC3339)), 0600); e != nil {
		log.Fatal(e)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"message": message, "version": os.Getenv("APP_VERSION")})
	})
	server := &http.Server{Addr: "127.0.0.1:" + port, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		server.Shutdown(c)
	}()
	fmt.Printf("sample App %s listening on %s\n", os.Getenv("APP_VERSION"), server.Addr)
	if e := server.ListenAndServe(); e != nil && e != http.ErrServerClosed {
		log.Fatal(e)
	}
}
