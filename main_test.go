package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestProbeDaemonIdentifiesServer verifies probeDaemon extracts serverInfo.name
// from a JSON-RPC initialize result served by a fake daemon.
func TestProbeDaemonIdentifiesServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"serverInfo":{"name":"airlock","version":"2.0.0-dev"}}}`))
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	name, ok := probeDaemon(addr)
	if !ok {
		t.Fatalf("probeDaemon ok = false, want true")
	}
	if name != "airlock" {
		t.Errorf("probeDaemon name = %q, want airlock", name)
	}
}

// TestProbeDaemonClosedPort verifies probeDaemon returns ok=false when nothing
// is listening on the address.
func TestProbeDaemonClosedPort(t *testing.T) {
	// Spin up then immediately close a server to obtain an almost-certainly-dead
	// address.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := strings.TrimPrefix(srv.URL, "http://")
	srv.Close()

	if name, ok := probeDaemon(addr); ok {
		t.Errorf("probeDaemon on closed port = (%q, true), want ok=false", name)
	}
}

// TestProbeDaemonNonAirlockResponse verifies a JSON response without a
// serverInfo.name is treated as not-an-airlock-daemon.
func TestProbeDaemonNonAirlockResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"hello":"world"}`))
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	if name, ok := probeDaemon(addr); ok {
		t.Errorf("probeDaemon on non-airlock = (%q, true), want ok=false", name)
	}
}
