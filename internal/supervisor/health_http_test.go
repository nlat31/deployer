package supervisor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"deployer/internal/model"
)

type httpHealthProcess struct{ Interface }

func (*httpHealthProcess) Status(context.Context, Spec) (Status, error) {
	return Status{Running: true, PID: 123}, nil
}
func TestHTTPHealthIgnoresApplicationInitializationAndProgress(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ready":false,"status":"initializing","progress":"unchanged"}`)
	}))
	defer server.Close()
	h := model.Health{Type: "http", Target: server.URL, TimeoutSeconds: 2, StableSeconds: 1}
	s := &httpHealthProcess{}
	if err := Healthy(context.Background(), s, Spec{}, h); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := Probe(context.Background(), s, Spec{}, h); err != nil {
			t.Fatal(err)
		}
	}
}
func TestHTTPHealthFailurePreservesApplicationError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		fmt.Fprint(w, `{"error":"checksum mismatch"}`)
	}))
	defer server.Close()
	err := Healthy(context.Background(), &httpHealthProcess{}, Spec{}, model.Health{Type: "http", Target: server.URL, TimeoutSeconds: 1, StableSeconds: 1})
	if err == nil || !strings.Contains(err.Error(), "503") || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatal(err)
	}
}
