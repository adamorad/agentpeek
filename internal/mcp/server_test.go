package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// echoHandler is a trivial stub that records invocations and echoes the method.
type echoHandler struct {
	called  int
	lastErr *RPCError
}

func (h *echoHandler) Handle(_ context.Context, method string, _ json.RawMessage) (any, *RPCError) {
	h.called++
	if h.lastErr != nil {
		return nil, h.lastErr
	}
	return map[string]any{"ok": true, "method": method}, nil
}

// startServer launches a Server on an ephemeral loopback port and returns it
// plus a teardown func. It blocks until the server is accepting connections.
func startServer(t *testing.T, h Handler, opts Options) (*Server, func()) {
	t.Helper()
	if opts.Addr == "" {
		opts.Addr = "127.0.0.1:0"
	}
	s := New(h, opts)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Start(ctx) }()

	// Wait for the listener to bind and accept healthz.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if s.Addr() != "" && !strings.HasSuffix(s.Addr(), ":0") {
			req, _ := http.NewRequest(http.MethodGet, "http://"+s.Addr()+"/healthz", nil)
			req.Host = "127.0.0.1"
			if resp, err := http.DefaultClient.Do(req); err == nil {
				resp.Body.Close()
				break
			}
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("server did not become ready")
		}
		time.Sleep(5 * time.Millisecond)
	}

	teardown := func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("server did not shut down")
		}
	}
	return s, teardown
}

// doPOST issues a POST to the server root with the given body and optional
// header mutations, returning status code and body.
func doPOST(t *testing.T, s *Server, body string, mut func(*http.Request)) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "http://"+s.Addr()+"/", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "127.0.0.1"
	req.Header.Set("Content-Type", "application/json")
	if mut != nil {
		mut(req)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestValidPOST_EchoesID(t *testing.T) {
	h := &echoHandler{}
	s, teardown := startServer(t, h, Options{})
	defer teardown()

	cases := []struct {
		name   string
		idJSON string
		wantID string
	}{
		{"number", `1`, `1`},
		{"string", `"abc"`, `"abc"`},
		{"null", `null`, `null`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"method":"ping","params":{}}`, tc.idJSON)
			status, resp := doPOST(t, s, body, nil)
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", status, resp)
			}
			var env struct {
				JSONRPC string          `json:"jsonrpc"`
				ID      json.RawMessage `json:"id"`
				Result  struct {
					OK     bool   `json:"ok"`
					Method string `json:"method"`
				} `json:"result"`
			}
			if err := json.Unmarshal([]byte(resp), &env); err != nil {
				t.Fatalf("unmarshal: %v; body=%s", err, resp)
			}
			if env.JSONRPC != "2.0" {
				t.Errorf("jsonrpc = %q, want 2.0", env.JSONRPC)
			}
			if string(env.ID) != tc.wantID {
				t.Errorf("id = %s, want %s", env.ID, tc.wantID)
			}
			if !env.Result.OK || env.Result.Method != "ping" {
				t.Errorf("result = %+v, want ok+ping", env.Result)
			}
		})
	}
}

func TestSecurity_HostOriginContentTypeBody(t *testing.T) {
	h := &echoHandler{}
	s, teardown := startServer(t, h, Options{MaxBodyBytes: 64})
	defer teardown()

	body := `{"jsonrpc":"2.0","id":1,"method":"ping","params":{}}`

	t.Run("bad host", func(t *testing.T) {
		status, _ := doPOST(t, s, body, func(r *http.Request) { r.Host = "evil.com" })
		if status != http.StatusForbidden {
			t.Errorf("status = %d, want 403", status)
		}
	})
	t.Run("bad host with port", func(t *testing.T) {
		status, _ := doPOST(t, s, body, func(r *http.Request) { r.Host = "evil.com:27183" })
		if status != http.StatusForbidden {
			t.Errorf("status = %d, want 403", status)
		}
	})
	t.Run("origin present", func(t *testing.T) {
		status, _ := doPOST(t, s, body, func(r *http.Request) { r.Header.Set("Origin", "http://evil.com") })
		if status != http.StatusForbidden {
			t.Errorf("status = %d, want 403", status)
		}
	})
	t.Run("wrong content-type", func(t *testing.T) {
		status, _ := doPOST(t, s, body, func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") })
		if status != http.StatusUnsupportedMediaType {
			t.Errorf("status = %d, want 415", status)
		}
	})
	t.Run("body over limit", func(t *testing.T) {
		big := `{"jsonrpc":"2.0","id":1,"method":"ping","params":{"x":"` + strings.Repeat("a", 200) + `"}}`
		status, _ := doPOST(t, s, big, nil)
		if status != http.StatusRequestEntityTooLarge {
			t.Errorf("status = %d, want 413", status)
		}
	})
	t.Run("charset content-type allowed", func(t *testing.T) {
		status, _ := doPOST(t, s, body, func(r *http.Request) {
			r.Header.Set("Content-Type", "application/json; charset=utf-8")
		})
		if status != http.StatusOK {
			t.Errorf("status = %d, want 200", status)
		}
	})
	t.Run("localhost host allowed", func(t *testing.T) {
		status, _ := doPOST(t, s, body, func(r *http.Request) { r.Host = "localhost:1234" })
		if status != http.StatusOK {
			t.Errorf("status = %d, want 200", status)
		}
	})
}

func TestMethodPathGuards(t *testing.T) {
	h := &echoHandler{}
	s, teardown := startServer(t, h, Options{})
	defer teardown()

	t.Run("GET root 405", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, "http://"+s.Addr()+"/", nil)
		req.Host = "127.0.0.1"
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("status = %d, want 405", resp.StatusCode)
		}
	})
	t.Run("unknown path 404", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, "http://"+s.Addr()+"/nope", strings.NewReader("{}"))
		req.Host = "127.0.0.1"
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("status = %d, want 404", resp.StatusCode)
		}
	})
}

func TestTokenAuth(t *testing.T) {
	h := &echoHandler{}
	s, teardown := startServer(t, h, Options{Token: "s3cret"})
	defer teardown()

	body := `{"jsonrpc":"2.0","id":1,"method":"ping","params":{}}`

	t.Run("no auth 401", func(t *testing.T) {
		status, _ := doPOST(t, s, body, nil)
		if status != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", status)
		}
	})
	t.Run("wrong token 401", func(t *testing.T) {
		status, _ := doPOST(t, s, body, func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer wrong")
		})
		if status != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", status)
		}
	})
	t.Run("correct token 200", func(t *testing.T) {
		status, _ := doPOST(t, s, body, func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer s3cret")
		})
		if status != http.StatusOK {
			t.Errorf("status = %d, want 200", status)
		}
	})
}

func TestNotificationInitialized(t *testing.T) {
	h := &echoHandler{}
	s, teardown := startServer(t, h, Options{})
	defer teardown()

	body := `{"jsonrpc":"2.0","method":"notifications/initialized"}`
	status, resp := doPOST(t, s, body, nil)
	if status != http.StatusAccepted {
		t.Errorf("status = %d, want 202", status)
	}
	if resp != "" {
		t.Errorf("body = %q, want empty", resp)
	}
	if h.called != 0 {
		t.Errorf("handler called %d times, want 0", h.called)
	}
}

func TestMalformedJSON_ParseError(t *testing.T) {
	h := &echoHandler{}
	s, teardown := startServer(t, h, Options{})
	defer teardown()

	status, resp := doPOST(t, s, `{not json`, nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (JSON-RPC error carries 200)", status)
	}
	var env struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   *RPCError       `json:"error"`
	}
	if err := json.Unmarshal([]byte(resp), &env); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, resp)
	}
	if env.Error == nil || env.Error.Code != -32700 {
		t.Errorf("error = %+v, want code -32700", env.Error)
	}
	if string(env.ID) != "null" {
		t.Errorf("id = %s, want null", env.ID)
	}
	if h.called != 0 {
		t.Errorf("handler called %d times, want 0", h.called)
	}
}

func TestBadJSONRPCVersion_ParseError(t *testing.T) {
	h := &echoHandler{}
	s, teardown := startServer(t, h, Options{})
	defer teardown()

	status, resp := doPOST(t, s, `{"jsonrpc":"1.0","id":1,"method":"ping"}`, nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	var env struct {
		Error *RPCError `json:"error"`
	}
	if err := json.Unmarshal([]byte(resp), &env); err != nil {
		t.Fatal(err)
	}
	if env.Error == nil || env.Error.Code != -32700 {
		t.Errorf("error = %+v, want code -32700", env.Error)
	}
}

func TestHealthz(t *testing.T) {
	h := &echoHandler{}
	s, teardown := startServer(t, h, Options{Token: "s3cret"})
	defer teardown()

	req, _ := http.NewRequest(http.MethodGet, "http://"+s.Addr()+"/healthz", nil)
	req.Host = "127.0.0.1"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if string(b) != "ok" {
		t.Errorf("body = %q, want ok", string(b))
	}

	t.Run("healthz bad host 403", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, "http://"+s.Addr()+"/healthz", nil)
		req.Host = "evil.com"
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("status = %d, want 403", resp.StatusCode)
		}
	})
}

func TestEnsureTokenFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "token")

	t.Run("create mints 0600 hex", func(t *testing.T) {
		tok, err := EnsureTokenFile(path, true)
		if err != nil {
			t.Fatal(err)
		}
		if len(tok) != 64 {
			t.Errorf("token len = %d, want 64 hex chars", len(tok))
		}
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("mode = %o, want 600", fi.Mode().Perm())
		}
	})

	t.Run("reading again returns same", func(t *testing.T) {
		first, err := EnsureTokenFile(path, true)
		if err != nil {
			t.Fatal(err)
		}
		second, err := EnsureTokenFile(path, false)
		if err != nil {
			t.Fatal(err)
		}
		if first != second {
			t.Errorf("tokens differ: %q vs %q", first, second)
		}
	})

	t.Run("create false absent returns empty", func(t *testing.T) {
		tok, err := EnsureTokenFile(filepath.Join(dir, "absent"), false)
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if tok != "" {
			t.Errorf("token = %q, want empty", tok)
		}
	})
}

func TestAddrAfterStart(t *testing.T) {
	h := &echoHandler{}
	s, teardown := startServer(t, h, Options{Addr: "127.0.0.1:0"})
	defer teardown()

	addr := s.Addr()
	if addr == "" || strings.HasSuffix(addr, ":0") {
		t.Errorf("Addr() = %q, want bound ephemeral addr", addr)
	}
	if !strings.HasPrefix(addr, "127.0.0.1:") {
		t.Errorf("Addr() = %q, want 127.0.0.1 prefix", addr)
	}
}
