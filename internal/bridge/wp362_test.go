package bridge

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var testTLSConfig *tls.Config

func init() {
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	ts.EnableHTTP2 = true
	ts.StartTLS()
	testTLSConfig = ts.TLS.Clone()
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ts.Certificate().Raw})
	ts.Close()
	if f, err := os.CreateTemp("", "lmmcp-test-ca-*.pem"); err == nil {
		_, _ = f.Write(pemBytes)
		_ = f.Close()
		_ = os.Setenv("SSL_CERT_FILE", f.Name())
	}
}

// P1-1:Committed 14-scenario replay matrix. Effect executions must equal 1 per written tool call in every scenario.
func TestReplayMatrixAllScenariosExecuteOnce(t *testing.T) {
	runSinglePostMode := func(t *testing.T, caseName string, wantEffects int64, handler func(w http.ResponseWriter, r *http.Request, id any, sid string, effects *atomic.Int64, sessions *sync.Map)) {
		t.Run(caseName, func(t *testing.T) {
			var effects, inits atomic.Int64
			var sessions sync.Map
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					w.WriteHeader(405)
					return
				}
				if r.Method == "DELETE" {
					w.WriteHeader(200)
					return
				}
				b, _ := io.ReadAll(r.Body)
				var m struct {
					ID     any    `json:"id"`
					Method string `json:"method"`
				}
				_ = json.Unmarshal(b, &m)
				w.Header().Set("Content-Type", "application/json")
				write := func(v any) {
					out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": v})
					_, _ = w.Write(out)
				}
				switch {
				case m.Method == "initialize":
					id := "S" + strconv.FormatInt(inits.Add(1), 10)
					sessions.Store(id, true)
					w.Header().Set("Mcp-Session-Id", id)
					write(map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "stub", "version": "1"}})
				case m.ID == nil:
					w.WriteHeader(202)
				case m.Method == "tools/list":
					write(map[string]any{"tools": []any{map[string]any{"name": "effect", "inputSchema": map[string]any{"type": "object"}}}})
				case m.Method == "tools/call":
					handler(w, r, m.ID, r.Header.Get("Mcp-Session-Id"), &effects, &sessions)
				}
			}))
			defer srv.Close()
			g, _ := New(Config{Servers: map[string]ServerConfig{"r": {URL: srv.URL, ProtocolVersion: "2025-11-25"}}}, Options{})
			defer g.Close()
			_ = request(g, "POST", "/v1/servers/r/tools/effect/call", `{"x":1}`)
			if effects.Load() != wantEffects {
				t.Fatalf("%s: effectExecutions=%d, want %d", caseName, effects.Load(), wantEffects)
			}
		})
	}

	// 1. Connection reset after request body was written
	runSinglePostMode(t, "1-conn-reset-after-write", 1, func(w http.ResponseWriter, r *http.Request, id any, sid string, effects *atomic.Int64, sessions *sync.Map) {
		effects.Add(1)
		conn, _, _ := w.(http.Hijacker).Hijack()
		if tc, ok := conn.(*net.TCPConn); ok {
			_ = tc.SetLinger(0)
		}
		_ = conn.Close()
	})

	// 2a. EOF mid-response (JSON Content-Length truncated)
	runSinglePostMode(t, "2a-eof-mid-response-json", 1, func(w http.ResponseWriter, r *http.Request, id any, sid string, effects *atomic.Int64, sessions *sync.Map) {
		effects.Add(1)
		conn, rw, _ := w.(http.Hijacker).Hijack()
		_, _ = rw.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 200\r\n\r\n{\"jsonrpc\":\"2.0\",\"id\":1")
		_ = rw.Flush()
		_ = conn.Close()
	})

	// 2b. EOF mid-response (SSE stream closed before result event)
	runSinglePostMode(t, "2b-eof-mid-response-sse", 1, func(w http.ResponseWriter, r *http.Request, id any, sid string, effects *atomic.Int64, sessions *sync.Map) {
		effects.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(": keepalive\n\n"))
		w.(http.Flusher).Flush()
		conn, _, _ := w.(http.Hijacker).Hijack()
		_ = conn.Close()
	})

	// 3a. 404 returned for expired session BEFORE execution on attempt 0, then executed once on attempt 1
	runSinglePostMode(t, "3a-404-before-execute-once", 1, func(w http.ResponseWriter, r *http.Request, id any, sid string, effects *atomic.Int64, sessions *sync.Map) {
		if sid == "S1" {
			sessions.Delete(sid)
			w.WriteHeader(404)
			return
		}
		effects.Add(1)
		out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": text("ok")})
		_, _ = w.Write(out)
	})

	// 4. 404 on GET (SSE stream) while a call is in flight
	t.Run("4-404-on-get-sse-while-call-in-flight", func(t *testing.T) {
		var effects atomic.Int64
		entered := make(chan struct{})
		getDone := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == "GET" {
				select {
				case <-entered:
				case <-time.After(300 * time.Millisecond):
				}
				w.WriteHeader(404)
				select {
				case <-getDone:
				default:
					close(getDone)
				}
				return
			}
			if r.Method == "DELETE" {
				w.WriteHeader(200)
				return
			}
			var m struct {
				ID     any    `json:"id"`
				Method string `json:"method"`
			}
			_ = json.NewDecoder(r.Body).Decode(&m)
			w.Header().Set("Content-Type", "application/json")
			write := func(v any) {
				out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": v})
				_, _ = w.Write(out)
			}
			switch {
			case m.Method == "initialize":
				w.Header().Set("Mcp-Session-Id", "S1")
				write(map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "stub", "version": "1"}})
			case m.ID == nil:
				w.WriteHeader(202)
			case m.Method == "tools/list":
				write(map[string]any{"tools": []any{map[string]any{"name": "effect", "inputSchema": map[string]any{"type": "object"}}}})
			case m.Method == "tools/call":
				effects.Add(1)
				close(entered)
				<-getDone
				write(text("ok"))
			}
		}))
		defer srv.Close()
		g, _ := New(Config{Servers: map[string]ServerConfig{"r": {URL: srv.URL, ProtocolVersion: "2025-11-25"}}}, Options{})
		defer g.Close()
		resp := request(g, "POST", "/v1/servers/r/tools/effect/call", `{}`)
		if resp.Status != 200 || effects.Load() != 1 {
			t.Fatalf("status=%d effects=%d", resp.Status, effects.Load())
		}
	})

	// 5. Session expiry racing a call
	t.Run("5-session-expiry-racing-call", func(t *testing.T) {
		s := newStub(t)
		s.holdFirst = true
		g := s.gateway(t, nil)
		_ = request(g, "GET", "/v1/servers/r/tools", "")
		s.mu.Lock()
		s.expireTool = "echo"
		s.mu.Unlock()
		aDone := make(chan Response, 1)
		go func() { aDone <- request(g, "POST", "/v1/servers/r/tools/effect/call", `{}`) }()
		<-s.entered
		_ = request(g, "POST", "/v1/servers/r/tools/echo/call", `{}`)
		close(s.holdRelease)
		<-aDone
		if s.effectCount() != 1 {
			t.Fatalf("effects=%d", s.effectCount())
		}
	})

	// 6. Two concurrent effectful calls with one expiry (2 distinct calls -> 1 execution each)
	t.Run("6-two-concurrent-calls-one-expiry", func(t *testing.T) {
		var effects, inits atomic.Int64
		bothEntered := make(chan struct{})
		var enteredCount atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "POST" {
				w.WriteHeader(405)
				return
			}
			var m struct {
				ID     any    `json:"id"`
				Method string `json:"method"`
			}
			_ = json.NewDecoder(r.Body).Decode(&m)
			w.Header().Set("Content-Type", "application/json")
			write := func(v any) {
				out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": v})
				_, _ = w.Write(out)
			}
			switch {
			case m.Method == "initialize":
				id := "S" + strconv.FormatInt(inits.Add(1), 10)
				w.Header().Set("Mcp-Session-Id", id)
				write(map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "stub", "version": "1"}})
			case m.ID == nil:
				w.WriteHeader(202)
			case m.Method == "tools/list":
				write(map[string]any{"tools": []any{map[string]any{"name": "effect", "inputSchema": map[string]any{"type": "object"}}}})
			case m.Method == "tools/call":
				n := enteredCount.Add(1)
				if n == 2 {
					close(bothEntered)
				}
				<-bothEntered
				if n == 1 {
					effects.Add(1)
					conn, _, _ := w.(http.Hijacker).Hijack()
					_ = conn.Close()
					return
				}
				if r.Header.Get("Mcp-Session-Id") == "S1" {
					w.WriteHeader(404)
					return
				}
				effects.Add(1)
				write(text("ok"))
			}
		}))
		defer srv.Close()
		g, _ := New(Config{Servers: map[string]ServerConfig{"r": {URL: srv.URL, ProtocolVersion: "2025-11-25"}}}, Options{})
		defer g.Close()
		_ = request(g, "GET", "/v1/servers/r/tools", "")
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _ = request(g, "POST", "/v1/servers/r/tools/effect/call", `{}`) }()
		go func() { defer wg.Done(); _ = request(g, "POST", "/v1/servers/r/tools/effect/call", `{}`) }()
		wg.Wait()
		if effects.Load() != 2 {
			t.Fatalf("effects=%d, want 2 (1 per call)", effects.Load())
		}
	})

	// 7. Server restart between calls (2 sequential calls -> 1 execution each)
	t.Run("7-server-restart-between-calls", func(t *testing.T) {
		s := newStub(t)
		g := s.gateway(t, nil)
		r1 := request(g, "POST", "/v1/servers/r/tools/effect/call", `{}`)
		s.mu.Lock()
		s.sessions = map[string]bool{}
		s.mu.Unlock()
		r2 := request(g, "POST", "/v1/servers/r/tools/effect/call", `{}`)
		if r1.Status != 200 || r2.Status != 200 || s.effectCount() != 2 {
			t.Fatalf("r1=%d r2=%d effects=%d", r1.Status, r2.Status, s.effectCount())
		}
	})

	// 8. TLS + HTTP/2 (h2) stream abort (RST_STREAM) after request body written
	t.Run("8-tls-h2-rst-stream-after-write", func(t *testing.T) {
		var effects atomic.Int64
		var protoSeen atomic.Value
		protoSeen.Store("")
		ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			protoSeen.Store(r.Proto)
			if r.Method != "POST" {
				w.WriteHeader(405)
				return
			}
			var m struct {
				ID     any    `json:"id"`
				Method string `json:"method"`
			}
			_ = json.NewDecoder(r.Body).Decode(&m)
			w.Header().Set("Content-Type", "application/json")
			write := func(v any) {
				out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": v})
				_, _ = w.Write(out)
			}
			switch {
			case m.Method == "initialize":
				w.Header().Set("Mcp-Session-Id", "S1")
				write(map[string]any{"protocolVersion": "2026-07-28", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "stub", "version": "1"}})
			case m.ID == nil:
				w.WriteHeader(202)
			case m.Method == "tools/list":
				write(map[string]any{"tools": []any{map[string]any{"name": "effect", "inputSchema": map[string]any{"type": "object"}}}})
			case m.Method == "tools/call":
				effects.Add(1)
				panic(http.ErrAbortHandler)
			}
		}))
		ts.TLS = testTLSConfig.Clone()
		ts.EnableHTTP2 = true
		ts.StartTLS()
		defer ts.Close()

		g, _ := New(Config{Servers: map[string]ServerConfig{"r": {URL: ts.URL, ProtocolVersion: "2026-07-28"}}}, Options{})
		defer g.Close()
		_ = request(g, "GET", "/v1/servers/r/tools", "")
		_ = request(g, "POST", "/v1/servers/r/tools/effect/call", `{}`)
		if effects.Load() != 1 {
			t.Fatalf("h2 abort: effects=%d", effects.Load())
		}
		if got := protoSeen.Load().(string); got != "HTTP/2.0" {
			t.Fatalf("TLS server negotiated %q, want HTTP/2.0 (ForceAttemptHTTP2 must be enabled)", got)
		}
	})

	// 9. Redirect responses (301, 302, 307, 308) after write
	for _, code := range []int{301, 302, 307, 308} {
		c := code
		runSinglePostMode(t, "9-redirect-"+strconv.Itoa(c)+"-after-write", 1, func(w http.ResponseWriter, r *http.Request, id any, sid string, effects *atomic.Int64, sessions *sync.Map) {
			effects.Add(1)
			http.Redirect(w, r, "/other", c)
		})
	}

	// 10. 5xx and 429 after write
	for _, code := range []int{500, 502, 503, 504, 429} {
		c := code
		runSinglePostMode(t, "10-status-"+strconv.Itoa(c)+"-after-write", 1, func(w http.ResponseWriter, r *http.Request, id any, sid string, effects *atomic.Int64, sessions *sync.Map) {
			effects.Add(1)
			w.WriteHeader(c)
		})
	}

	// 11. Slow / cancelled call
	t.Run("11-slow-cancelled-call", func(t *testing.T) {
		s := newStub(t)
		s.holdFirst = true
		g := s.gateway(t, nil)
		_ = request(g, "GET", "/v1/servers/r/tools", "")
		done := make(chan Response, 1)
		go func() {
			done <- g.Dispatch(context.Background(), Request{ID: "cancel-me", Method: "POST", Path: "/v1/servers/r/tools/effect/call", Body: json.RawMessage(`{}`)})
		}()
		<-s.entered
		_ = request(g, "DELETE", "/v1/requests/cancel-me", "")
		resp := <-done
		close(s.holdRelease)
		if resp.Status != 499 || s.effectCount() != 1 {
			t.Fatalf("status=%d effects=%d", resp.Status, s.effectCount())
		}
	})

	// 12. Auto-fallback path (POST /mcp initialize fails -> GET /sse)
	t.Run("12-auto-fallback-post-404-then-sse", func(t *testing.T) {
		var effects atomic.Int64
		out := make(chan string, 16)
		mux := http.NewServeMux()
		mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) })
		mux.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			_, _ = w.Write([]byte("event: endpoint\ndata: /messages?s=1\n\n"))
			w.(http.Flusher).Flush()
			for {
				select {
				case m := <-out:
					_, _ = w.Write([]byte("event: message\ndata: " + m + "\n\n"))
					w.(http.Flusher).Flush()
				case <-r.Context().Done():
					return
				}
			}
		})
		mux.HandleFunc("/messages", func(w http.ResponseWriter, r *http.Request) {
			var m struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			_ = json.NewDecoder(r.Body).Decode(&m)
			w.WriteHeader(202)
			switch m.Method {
			case "initialize":
				out <- `{"jsonrpc":"2.0","id":` + string(m.ID) + `,"result":{"protocolVersion":"2024-11-05","capabilities":{"tools":{}},"serverInfo":{"name":"s","version":"1"}}}`
			case "tools/list":
				out <- `{"jsonrpc":"2.0","id":` + string(m.ID) + `,"result":{"tools":[{"name":"effect","inputSchema":{"type":"object"}}]}}`
			case "tools/call":
				effects.Add(1)
				out <- `{"jsonrpc":"2.0","id":` + string(m.ID) + `,"result":{"content":[{"type":"text","text":"ok"}]}}`
			}
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()
		g, _ := New(Config{Servers: map[string]ServerConfig{"r": {URL: srv.URL + "/mcp", Type: "auto"}}}, Options{})
		defer g.Close()
		resp := request(g, "POST", "/v1/servers/r/tools/effect/call", `{}`)
		if resp.Status != 200 || effects.Load() != 1 {
			t.Fatalf("status=%d effects=%d", resp.Status, effects.Load())
		}
	})

	// 13 & 14. P1-1: tools/call arguments containing notifications/cancelled text, nested objects, unicode escapes, and whitespace variations
	// must NEVER cause the call to be unclassified as a POST (never replayed on concurrent 404, and never detached from per-call context on cancel).
	payloads := map[string]string{
		"direct-nested":   `{"a":{"method":"notifications/cancelled"}}`,
		"deep-nested":     `{"outer":{"inner":{"jsonrpc":"2.0","method":"notifications/cancelled"}}}`,
		"unicode-escaped": `{"a":{"method":"\u006eotifications/cancelled"}}`,
		"whitespace":      `{ "a" : { "method" : "notifications/cancelled" } }`,
	}
	for name, payload := range payloads {
		t.Run("13-no-replay-on-arg-"+name, func(t *testing.T) {
			s := newStub(t)
			s.holdFirst = true
			g := s.gateway(t, nil)
			_ = request(g, "GET", "/v1/servers/r/tools", "")
			s.mu.Lock()
			s.expireTool = "echo"
			s.mu.Unlock()
			aDone := make(chan Response, 1)
			go func() {
				aDone <- request(g, "POST", "/v1/servers/r/tools/effect/call", payload)
			}()
			<-s.entered
			_ = request(g, "POST", "/v1/servers/r/tools/echo/call", `{}`)
			close(s.holdRelease)
			<-aDone
			if n := s.effectCount(); n != 1 {
				t.Fatalf("%s: effect executed %d times (want 1)", name, n)
			}
		})

		t.Run("14-cancel-works-on-arg-"+name, func(t *testing.T) {
			s := newStub(t)
			s.holdFirst = true
			g := s.gateway(t, nil)
			_ = request(g, "GET", "/v1/servers/r/tools", "")
			done := make(chan Response, 1)
			go func() {
				done <- g.Dispatch(context.Background(), Request{ID: "c-" + name, Method: "POST", Path: "/v1/servers/r/tools/effect/call", Body: json.RawMessage(payload)})
			}()
			<-s.entered
			_ = request(g, "DELETE", "/v1/requests/c-"+name, "")
			select {
			case r := <-done:
				if r.Status != 499 {
					t.Fatalf("%s: status=%d body=%s", name, r.Status, body(r))
				}
			case <-time.After(2 * time.Second):
				close(s.holdRelease)
				t.Fatalf("%s: cancelling tools/call hung because request context was replaced with context.WithoutCancel", name)
			}
			close(s.holdRelease)
			if n := s.effectCount(); n != 1 {
				t.Fatalf("%s: effect executed %d times", name, n)
			}
		})
	}
}

// P1-1 unit test: cancelNotification parses the JSON-RPC envelope structurally.
func TestCancelNotificationStructuralEnvelope(t *testing.T) {
	reqWithBody := func(method, body string) *http.Request {
		r, _ := http.NewRequest(method, "http://127.0.0.1/mcp", strings.NewReader(body))
		return r
	}
	cases := []struct {
		name string
		req  *http.Request
		want bool
	}{
		{"valid-compact", reqWithBody("POST", `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`), true},
		{"valid-whitespace", reqWithBody("POST", " \n\t { \"jsonrpc\" : \"2.0\" , \"params\" : { \"requestId\" : 1 , \"pad\" : \"" + strings.Repeat("x", 500) + "\" } , \"method\" : \"notifications/cancelled\" } "), true},
		{"valid-unicode-escape", reqWithBody("POST", `{"jsonrpc":"2.0","method":"notifications/\u0063ancelled"}`), true},
		{"get-method", reqWithBody("GET", `{"jsonrpc":"2.0","method":"notifications/cancelled"}`), false},
		{"has-numeric-id", reqWithBody("POST", `{"jsonrpc":"2.0","id":1,"method":"notifications/cancelled"}`), false},
		{"has-string-id", reqWithBody("POST", `{"jsonrpc":"2.0","id":"abc","method":"notifications/cancelled"}`), false},
		{"has-null-id", reqWithBody("POST", `{"jsonrpc":"2.0","id":null,"method":"notifications/cancelled"}`), false},
		{"wrong-jsonrpc-version", reqWithBody("POST", `{"jsonrpc":"1.0","method":"notifications/cancelled"}`), false},
		{"tools-call-with-cancel-in-args", reqWithBody("POST", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"effect","arguments":{"a":{"method":"notifications/cancelled"}}}}`), false},
		{"tools-call-with-escaped-cancel-in-args", reqWithBody("POST", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"effect","arguments":{"a":{"\u006dethod":"notifications/cancelled"}}}}`), false},
		{"malformed-json", reqWithBody("POST", `{"jsonrpc":"2.0","method":"notifications/cancelled"`), false},
		{"trailing-garbage", reqWithBody("POST", `{"jsonrpc":"2.0","method":"notifications/cancelled"} trailing`), false},
	}
	for _, c := range cases {
		if got := cancelNotification(c.req); got != c.want {
			t.Errorf("%s: cancelNotification=%v, want %v", c.name, got, c.want)
		}
	}

	// Kills cancel-notice-bound-to-lifetime: a notifications/cancelled POST sent when both req.Context() and
	// state.ctx are already cancelled still reaches the server because RoundTrip binds it to context.WithoutCancel and state.abort.
	var gotCancel atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCancel.Store(true)
		w.WriteHeader(202)
	}))
	defer srv.Close()
	state := originState(t, srv.URL)
	state.abort = context.Background()
	state.cancel() // session lifetime context is already cancelled
	callCtx, callCancel := context.WithCancel(context.Background())
	callCancel() // call context is also already cancelled
	req, _ := http.NewRequestWithContext(callCtx, "POST", srv.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`))
	res, err := state.RoundTrip(req)
	if err != nil {
		t.Fatalf("notifications/cancelled failed when call/lifetime ctx was cancelled: %v", err)
	}
	res.Body.Close()
	if !gotCancel.Load() {
		t.Fatal("server did not receive notifications/cancelled")
	}
}

// P1-2: Full table-driven address policy spelling matrix across URL time and dial time.
func TestAddressPolicyFullSpellingMatrix(t *testing.T) {
	cases := []struct {
		name           string
		url            string
		dialAddr       string
		loopLit        bool
		wantURLDef     bool
		wantURLNoPriv  bool
		wantDialDef    bool
		wantDialNoPriv bool
	}{
		// Cloud metadata & link-local (always refused)
		{"metadata-v4", "https://169.254.169.254/mcp", "169.254.169.254:443", false, false, false, false, false},
		{"link-local-v4", "https://169.254.0.1/mcp", "169.254.0.1:443", false, false, false, false, false},
		{"ipv4-mapped-v6-metadata", "https://[::ffff:169.254.169.254]/mcp", "[::ffff:169.254.169.254]:443", false, false, false, false, false},
		{"link-local-v6", "https://[fe80::1]/mcp", "[fe80::1]:443", false, false, false, false, false},
		{"link-local-v6-upper", "https://[FE80::1]/mcp", "[FE80::1]:443", false, false, false, false, false},
		{"aws-v6-metadata", "https://[fd00:ec2::254]/mcp", "[fd00:ec2::254]:443", false, false, false, false, false},
		{"aws-v6-metadata-upper", "https://[FD00:EC2::254]/mcp", "[FD00:EC2::254]:443", false, false, false, false, false},
		{"unspecified-v4", "https://0.0.0.0/mcp", "0.0.0.0:443", false, false, false, false, false},
		{"unspecified-v6", "https://[::]/mcp", "[::]:443", false, false, false, false, false},
		{"link-local-multicast-v4", "https://224.0.0.1/mcp", "224.0.0.1:443", false, false, false, false, false},
		{"link-local-multicast-v6", "https://[ff02::1]/mcp", "[ff02::1]:443", false, false, false, false, false},
		{"link-local-multicast-v6-zone", "https://[ff02::1%25lo]/mcp", "[ff02::1%lo]:443", false, false, false, false, false},

		// IPv6 zone IDs on forbidden addresses (must be refused at both URL time and dial time)
		{"zone-id-link-local-lo", "https://[fe80::1%25lo]/mcp", "[fe80::1%lo]:443", false, false, false, false, false},
		{"zone-id-link-local-eth0", "https://[fe80::1%25eth0]/mcp", "[fe80::1%eth0]:443", false, false, false, false, false},
		{"zone-id-aws-v6-metadata", "https://[fd00:ec2::254%25lo]/mcp", "[fd00:ec2::254%lo]:443", false, false, false, false, false},
		{"zone-id-v4-mapped-metadata", "https://[::ffff:169.254.169.254%25lo]/mcp", "[::ffff:169.254.169.254%lo]:443", false, false, false, false, false},
		{"zone-id-unspecified-v6", "https://[::%25lo]/mcp", "[::%lo]:443", false, false, false, false, false},

		// IPv6 zone IDs on private/loopback addresses (allowed when allowPrivate=true, refused when allowPrivate=false)
		{"zone-id-loopback-v6", "https://[::1%25lo]/mcp", "[::1%lo]:443", false, true, false, true, false},
		{"zone-id-ula-v6", "https://[fd00::1%25lo]/mcp", "[fd00::1%lo]:443", false, true, false, true, false},

		// Trailing-dot IP literals
		{"trailing-dot-metadata", "https://169.254.169.254./mcp", "169.254.169.254.:443", false, false, false, false, false},
		{"trailing-dot-rfc1918", "https://10.0.0.1./mcp", "10.0.0.1.:443", false, true, false, true, false},
		{"trailing-dot-loopback-https", "https://127.0.0.1./mcp", "127.0.0.1.:443", false, true, false, true, false},
		{"trailing-dot-loopback-http", "http://127.0.0.1.:8080/mcp", "127.0.0.1.:8080", false, false, false, true, false},

		// Decimal / octal / hex IPv4 spellings
		{"decimal-ipv4-metadata", "https://2852039166/mcp", "2852039166:443", false, false, false, false, false},
		{"octal-ipv4-metadata", "https://0251.0376.0251.0376/mcp", "0251.0376.0251.0376:443", false, false, false, false, false},
		{"hex-ipv4-metadata", "https://0xa9fea9fe/mcp", "0xa9fea9fe:443", false, false, false, false, false},
		{"short-ipv4-metadata", "https://169.254.43518/mcp", "169.254.43518:443", false, false, false, false, false},
		{"decimal-ipv4-private", "https://167772161/mcp", "167772161:443", false, true, false, true, false},
		{"hex-ipv4-private", "https://0x0a000001/mcp", "0x0a000001:443", false, true, false, true, false},
		{"decimal-ipv4-loopback-http-refused", "http://2130706433:8080/mcp", "2130706433:8080", false, false, false, true, false},
		{"hex-ipv4-loopback-http-refused", "http://0x7f.0.0.1:8080/mcp", "0x7f.0.0.1:8080", false, false, false, true, false},
		{"octal-ipv4-loopback-http-refused", "http://0177.0.0.1:8080/mcp", "0177.0.0.1:8080", false, false, false, true, false},

		// Private ranges (RFC1918, CGNAT, ULA, IPv4-mapped private, loopback)
		{"rfc1918-10", "https://10.0.0.1/mcp", "10.0.0.1:443", false, true, false, true, false},
		{"rfc1918-172-16", "https://172.16.0.1/mcp", "172.16.0.1:443", false, true, false, true, false},
		{"rfc1918-192-168", "https://192.168.1.1/mcp", "192.168.1.1:443", false, true, false, true, false},
		{"cgnat-100-64", "https://100.64.0.1/mcp", "100.64.0.1:443", false, true, false, true, false},
		{"cgnat-100-127", "https://100.127.255.254/mcp", "100.127.255.254:443", false, true, false, true, false},
		{"ula-fc00", "https://[fc00::1]/mcp", "[fc00::1]:443", false, true, false, true, false},
		{"ula-fd00", "https://[fd00::1]/mcp", "[fd00::1]:443", false, true, false, true, false},
		{"ipv4-mapped-private", "https://[::ffff:10.0.0.1]/mcp", "[::ffff:10.0.0.1]:443", false, true, false, true, false},
		{"https-loopback-v4", "https://127.0.0.1/mcp", "127.0.0.1:443", false, true, false, true, false},
		{"https-loopback-v6", "https://[::1]/mcp", "[::1]:443", false, true, false, true, false},
		{"http-literal-loopback-v4", "http://127.0.0.1:8080/mcp", "127.0.0.1:8080", true, true, true, true, true},
		{"http-literal-loopback-v6", "http://[::1]:8080/mcp", "[::1]:8080", true, true, true, true, true},
		{"http-literal-loopback-v4-mapped", "http://[::ffff:127.0.0.1]:8080/mcp", "[::ffff:127.0.0.1]:8080", true, true, true, true, true},

		// Public DNS names, IDN, uppercase, trailing dot on domain
		{"public-domain", "https://example.com/mcp", "93.184.216.34:443", false, true, true, true, true},
		{"public-domain-uppercase", "HTTPS://EXAMPLE.COM/mcp", "93.184.216.34:443", false, true, true, true, true},
		{"idn-punycode", "https://xn--mnchen-3ya.de/mcp", "93.184.216.34:443", false, true, true, true, true},
		{"idn-unicode", "https://münchen.de/mcp", "93.184.216.34:443", false, true, true, true, true},
		{"trailing-dot-localhost-dns", "https://localhost./mcp", "127.0.0.1:443", false, true, true, true, false},
		{"unparseable-dial-host-fails-closed", "https://example.com/mcp", "not-an-ip:443", false, true, true, false, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, errDef := networkURL(c.url, true)
			if got := (errDef == nil); got != c.wantURLDef {
				t.Errorf("networkURL(%q, true) allowed=%v, want %v (err=%v)", c.url, got, c.wantURLDef, errDef)
			}
			_, errNoPriv := networkURL(c.url, false)
			if got := (errNoPriv == nil); got != c.wantURLNoPriv {
				t.Errorf("networkURL(%q, false) allowed=%v, want %v (err=%v)", c.url, got, c.wantURLNoPriv, errNoPriv)
			}
			errDialDef := dialControl(true, c.loopLit, nil)("tcp", c.dialAddr, nil)
			if got := (errDialDef == nil); got != c.wantDialDef {
				t.Errorf("dialControl(true, %v)(%q) allowed=%v, want %v (err=%v)", c.loopLit, c.dialAddr, got, c.wantDialDef, errDialDef)
			}
			errDialNoPriv := dialControl(false, c.loopLit, nil)("tcp", c.dialAddr, nil)
			if got := (errDialNoPriv == nil); got != c.wantDialNoPriv {
				t.Errorf("dialControl(false, %v)(%q) allowed=%v, want %v (err=%v)", c.loopLit, c.dialAddr, got, c.wantDialNoPriv, errDialNoPriv)
			}
		})
	}
}

// P2-1: Standalone GET handling and watchChanges are consistent for every configured protocol version, including omitted protocolVersion.
func TestNotificationStreamOmittedProtocolVersionAndOverflow(t *testing.T) {
	t.Run("omitted-protocol-version-flat-memory-and-list-changed", func(t *testing.T) {
		s := newStub(t)
		chunk := []byte(strings.Repeat("A", 1<<20))
		streamed := make(chan struct{})
		s.onGET = func(w http.ResponseWriter, r *http.Request) {
			defer close(streamed)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			// Stream a single 80 MiB line without newline (which would blow up go-sdk's standalone SSE scanner if DisableStandaloneSSE were false)
			_, _ = w.Write([]byte("data: "))
			for i := 0; i < 80; i++ {
				_, _ = w.Write(chunk)
				w.(http.Flusher).Flush()
				if r.Context().Err() != nil {
					return
				}
			}
			_, _ = w.Write([]byte("\n\n"))
			// Then send a valid list_changed event without space after colon
			_, _ = w.Write([]byte("data:{\"jsonrpc\":\"2.0\",\"method\":\"notifications/tools/list_changed\"}\n\n"))
			w.(http.Flusher).Flush()
		}
		events := make(chan Event, 10)
		// Note: ProtocolVersion is OMITTED ("")
		g, err := New(Config{Servers: map[string]ServerConfig{"r": {URL: s.srv.URL + "/mcp"}}}, Options{Event: func(e Event) {
			if e.Kind == "list_changed" {
				select {
				case events <- e:
				default:
				}
			}
		}})
		if err != nil {
			t.Fatal(err)
		}
		defer g.Close()

		runtime.GC()
		var base runtime.MemStats
		runtime.ReadMemStats(&base)
		var peak atomic.Uint64
		stop := make(chan struct{})
		go func() {
			for {
				select {
				case <-stop:
					return
				case <-time.After(5 * time.Millisecond):
					var m runtime.MemStats
					runtime.ReadMemStats(&m)
					if m.HeapAlloc > peak.Load() {
						peak.Store(m.HeapAlloc)
					}
				}
			}
		}()

		if r := request(g, "GET", "/v1/servers/r/tools", ""); r.Status != 200 {
			t.Fatal(body(r))
		}
		<-streamed
		close(stop)
		if grown := int64(peak.Load()) - int64(base.HeapAlloc); grown > 64<<20 {
			t.Fatalf("omitted protocolVersion: heap grew by %d MiB while streaming 80 MiB single line", grown>>20)
		}
		select {
		case <-events:
		case <-time.After(5 * time.Second):
			t.Fatal("omitted protocolVersion: list_changed was not delivered")
		}
	})

	// Kills watch-overflow-ignored: a 4096-byte prefix that happens to be valid JSON followed by extra bytes on the same SSE event must be rejected as overflow.
	t.Run("overflow-prefix-is-not-classified-as-list-changed", func(t *testing.T) {
		s := newStub(t)
		validPrefix := `{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}`
		padded4096 := validPrefix + strings.Repeat(" ", 4096-len(validPrefix))
		sent := make(chan struct{})
		s.onGET = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			// First event: 4096 bytes of valid JSON + 100 extra bytes -> overflow=true -> must NOT trigger list_changed!
			_, _ = w.Write([]byte("data: " + padded4096 + strings.Repeat("X", 100) + "\n\n"))
			// Also a JSON-RPC request with "id":1 and method notifications/tools/list_changed -> must NOT trigger list_changed!
			_, _ = w.Write([]byte("data: {\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"notifications/tools/list_changed\"}\n\n"))
			w.(http.Flusher).Flush()
			close(sent)
			<-r.Context().Done()
		}
		var count atomic.Int32
		g, _ := New(Config{Servers: map[string]ServerConfig{"r": {URL: s.srv.URL + "/mcp", ProtocolVersion: "2025-11-25"}}}, Options{Event: func(e Event) {
			if e.Kind == "list_changed" {
				count.Add(1)
			}
		}})
		defer g.Close()
		if r := request(g, "GET", "/v1/servers/r/tools", ""); r.Status != 200 {
			t.Fatal(body(r))
		}
		<-sent
		time.Sleep(150 * time.Millisecond)
		if count.Load() != 0 {
			t.Fatalf("overflowed or id-bearing event triggered list_changed %d times", count.Load())
		}
	})
}

// P2-2: g.get() re-checks g.closed after acquiring <-e.gate so no orphan stdio child can start after Close().
func TestStdioGetVsCloseNoOrphanChild(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Servers: map[string]ServerConfig{"mock": {
		Command: exe,
		Args:    []string{"-test.run=TestMockProcess"},
		Env:     map[string]string{"LMMCP_TEST_SERVER": "1"},
	}}}
	g, err := New(cfg, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if r := request(g, "GET", "/v1/servers/mock/tools", ""); r.Status != 200 {
		t.Fatal(body(r))
	}
	e := g.entries["mock"]
	<-e.gate // hold gate while Close() and a concurrent get() queue up on <-e.gate

	closeDone := make(chan struct{})
	go func() {
		g.Close()
		close(closeDone)
	}()
	time.Sleep(20 * time.Millisecond)

	// Simulate a goroutine that passed the initial g.mu check in g.get() just before g.Close() set g.closed=true,
	// and is now waiting on <-e.gate behind g.Close().
	g.mu.Lock()
	g.closed = false
	g.mu.Unlock()
	getDone := make(chan error, 1)
	go func() {
		_, getErr := g.get(context.Background(), "mock")
		getDone <- getErr
	}()
	time.Sleep(20 * time.Millisecond)
	g.mu.Lock()
	g.closed = true
	g.mu.Unlock()

	e.gate <- struct{}{}
	<-closeDone
	getErr := <-getDone

	<-e.gate
	leakedProc := e.process
	leakedSess := e.session
	e.gate <- struct{}{}
	if leakedProc != nil {
		_ = leakedProc.Close()
	}
	if getErr == nil || leakedProc != nil || leakedSess != nil || g.StartCount() != 1 {
		t.Fatalf("orphan stdio child started after Close(): getErr=%v leakedProc=%v starts=%d", getErr, leakedProc != nil, g.StartCount())
	}

	// Also stress concurrent Dispatch vs Close under -race.
	g2, err := New(cfg, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = request(g2, "GET", "/v1/servers/mock/tools", "")
		}()
	}
	g2.Close()
	wg.Wait()
	if e2 := g2.entries["mock"]; e2 != nil {
		<-e2.gate
		p2, s2 := e2.process, e2.session
		e2.gate <- struct{}{}
		if p2 != nil || s2 != nil {
			t.Fatalf("concurrent race left process=%v session=%v after Close()", p2 != nil, s2 != nil)
		}
	}
}

// P2-3: Repo hygiene check fails if any __pycache__ or .pyc/.pyo/.pyd file is tracked in git.
func findTrackedBytecode(paths []string) []string {
	var bad []string
	for _, p := range paths {
		if strings.Contains(p, "__pycache__/") || strings.HasSuffix(p, ".pyc") || strings.HasSuffix(p, ".pyo") || strings.HasSuffix(p, ".pyd") {
			bad = append(bad, p)
		}
	}
	return bad
}

func TestRepoHygieneNoTrackedBytecode(t *testing.T) {
	if got := findTrackedBytecode([]string{"scripts/e2e.py", "scripts/__pycache__/e2e.cpython-312.pyc"}); len(got) != 1 {
		t.Fatalf("detector control failed: %v", got)
	}
	out, err := exec.Command("git", "ls-files").Output()
	if err != nil {
		t.Skip("not a git checkout")
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if bad := findTrackedBytecode(lines); len(bad) > 0 {
		t.Fatalf("tracked bytecode or cache files found: %v", bad)
	}
}

// P2-4: Custom http.Transport settings preserve ForceAttemptHTTP2 and idle pool defaults while keeping Proxy: nil and dialControl.
func TestRemoteTransportSettings(t *testing.T) {
	tr := newRemoteTransport(true, false, nil)
	if tr.Proxy != nil {
		t.Fatal("Proxy must be nil")
	}
	if tr.DialContext == nil {
		t.Fatal("DialContext must be set")
	}
	if !tr.ForceAttemptHTTP2 {
		t.Fatal("ForceAttemptHTTP2 must be true so HTTPS connections can negotiate HTTP/2")
	}
	if tr.MaxIdleConns != 100 || tr.IdleConnTimeout != 90*time.Second || tr.TLSHandshakeTimeout != 10*time.Second || tr.ExpectContinueTimeout != 1*time.Second {
		t.Fatalf("unexpected transport pool defaults: MaxIdleConns=%d IdleConnTimeout=%v TLSHandshakeTimeout=%v ExpectContinueTimeout=%v",
			tr.MaxIdleConns, tr.IdleConnTimeout, tr.TLSHandshakeTimeout, tr.ExpectContinueTimeout)
	}
}

// P2-5 mutation killers for session replacement under active calls, DELETE before SDK close, DELETE protocol version header, RoundTrip transport error trace, and RPC error mapping.
func TestReplaceUnderRunningCallsClosesRetiredSession(t *testing.T) {
	s := newStub(t)
	var deletes atomic.Int32
	var releaseOnce sync.Once
	inner := s.srv.Config.Handler
	s.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" {
			if deletes.Add(1) >= 2 {
				releaseOnce.Do(func() { close(s.slowRelease) })
			}
		}
		inner.ServeHTTP(w, r)
	})
	g := s.gateway(t, nil)
	if r := request(g, "GET", "/v1/servers/r/tools", ""); r.Status != 200 {
		t.Fatal(body(r))
	}
	e := g.entries["r"]
	s1 := e.session

	// Start a slow call on s1 so e.users[s1] == 1.
	slowDone := make(chan Response, 1)
	go func() {
		slowDone <- request(g, "POST", "/v1/servers/r/tools/slow/call", `{}`)
	}()
	<-s.slowEntered

	// Mark s1 broken (as a transport error on a concurrent call does) and run a new call so dropCurrent retires s1 while e.users[s1] > 0.
	g.markBroken(e, s1)
	if r := request(g, "GET", "/v1/servers/r/tools", ""); r.Status != 200 {
		t.Fatal(body(r))
	}
	g.mu.Lock()
	_, inRetired := e.retired[s1]
	g.mu.Unlock()
	if !inRetired {
		releaseOnce.Do(func() { close(s.slowRelease) })
		<-slowDone
		t.Fatal("active broken session s1 was not placed in e.retired when replaced")
	}

	// Call g.invalidate(e) while slow is STILL blocked on s1 (g.ctx is not cancelled, so g.release cannot run):
	// only invalidate's loop over e.retired can close s1 and send its DELETE.
	g.invalidate(e)
	g.closing.Wait()
	got := deletes.Load()
	releaseOnce.Do(func() { close(s.slowRelease) })
	<-slowDone
	if got < 2 {
		t.Fatalf("expected DELETE for both retired and current sessions during invalidate(), got %d", got)
	}
}

func TestDeleteCarriesProtocolVersionHeader(t *testing.T) {
	var deleteProto atomic.Value
	deleteProto.Store("")
	s := newStub(t)
	inner := s.srv.Config.Handler
	s.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" {
			deleteProto.Store(r.Header.Get("Mcp-Protocol-Version"))
		}
		inner.ServeHTTP(w, r)
	})
	g := s.gateway(t, nil)
	if r := request(g, "GET", "/v1/servers/r/tools", ""); r.Status != 200 {
		t.Fatal(body(r))
	}
	rem := g.entries["r"].remote
	g.Close()
	if !rem.skipDelete.Load() {
		t.Fatal("remote.terminate did not run before session.Close()")
	}
	if got := deleteProto.Load().(string); got != "2025-11-25" {
		t.Fatalf("DELETE Mcp-Protocol-Version header = %q, want 2025-11-25", got)
	}
}

func TestRoundTripIncrementsOtherOnTransportError(t *testing.T) {
	state := originState(t, "http://127.0.0.1:8080")
	state.base = &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("synthetic transport failure")
	}}
	var tr callTrace
	ctx := context.WithValue(context.Background(), traceKey{}, &tr)
	req, _ := http.NewRequestWithContext(ctx, "POST", "http://127.0.0.1:8080/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call"}`))
	_, err := state.RoundTrip(req)
	if err == nil {
		t.Fatal("expected transport error")
	}
	if tr.sent.Load() != 1 || tr.other.Load() != 1 || tr.mayResend(true) {
		t.Fatalf("sent=%d other=%d mayResend=%v", tr.sent.Load(), tr.other.Load(), tr.mayResend(true))
	}
}

// P3-3 and P2-5: JSON-RPC error classification for -32601, -32602 resource missing, and -32602 "unknown argument" on prompt/resource.
func TestJSONRPCErrorClassificationDetails(t *testing.T) {
	cases := []struct {
		name     string
		kind     string
		rpcCode  int
		msg      string
		wantHTTP int
		wantCode string
	}{
		{"method-not-found-tool", "tool", -32601, "Method not found: tools/call", 404, "METHOD_NOT_SUPPORTED"},
		{"method-not-found-list", "list", -32601, "Method not found: resources/list", 404, "METHOD_NOT_SUPPORTED"},
		{"resource-not-found-32602", "resource", -32602, "Resource not found: mock://missing", 404, "RESOURCE_NOT_FOUND"},
		{"unknown-resource-32602", "resource", -32602, "unknown resource: mock://missing", 404, "RESOURCE_NOT_FOUND"},
		{"unknown-prompt-32602", "prompt", -32602, "unknown prompt: nope", 404, "PROMPT_NOT_FOUND"},
		{"prompt-not-found-32602", "prompt", -32602, "Prompt not found: nope", 404, "PROMPT_NOT_FOUND"},
		{"prompt-unknown-argument-is-400", "prompt", -32602, "unknown argument: foo", 400, "INVALID_PARAMS"},
		{"resource-unknown-parameter-is-400", "resource", -32602, "unknown parameter: version", 400, "INVALID_PARAMS"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newStub(t)
			s.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" {
					w.WriteHeader(405)
					return
				}
				var m struct {
					ID     any    `json:"id"`
					Method string `json:"method"`
				}
				_ = json.NewDecoder(r.Body).Decode(&m)
				w.Header().Set("Content-Type", "application/json")
				if m.Method == "initialize" {
					w.Header().Set("Mcp-Session-Id", "S1")
					b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{"tools": map[string]any{}, "prompts": map[string]any{}, "resources": map[string]any{}}, "serverInfo": map[string]any{"name": "stub", "version": "1"}}})
					_, _ = w.Write(b)
					return
				}
				if m.ID == nil {
					w.WriteHeader(202)
					return
				}
				if m.Method == "tools/list" && c.kind == "tool" {
					b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{"tools": []any{map[string]any{"name": "t", "inputSchema": map[string]any{"type": "object"}}}}})
					_, _ = w.Write(b)
					return
				}
				b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": c.rpcCode, "message": c.msg}})
				_, _ = w.Write(b)
			})
			g := s.gateway(t, nil)
			var resp Response
			switch c.kind {
			case "resource":
				resp = g.Dispatch(context.Background(), Request{Method: "GET", Path: "/v1/servers/r/resources/read", Query: map[string]string{"uri": "mock://x"}})
			case "prompt":
				resp = request(g, "POST", "/v1/servers/r/prompts/p", `{}`)
			case "tool":
				resp = request(g, "POST", "/v1/servers/r/tools/t/call", `{}`)
			case "list":
				resp = request(g, "GET", "/v1/servers/r/resources", "")
			}
			var out struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			_ = json.Unmarshal([]byte(body(resp)), &out)
			if resp.Status != c.wantHTTP || out.Error.Code != c.wantCode {
				t.Fatalf("%s: got status=%d code=%s, want status=%d code=%s (body=%s)", c.name, resp.Status, out.Error.Code, c.wantHTTP, c.wantCode, body(resp))
			}
		})
	}
}

var _ = bytes.NewReader
