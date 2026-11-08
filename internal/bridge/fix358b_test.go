package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRetryDecisionTruthTable(t *testing.T) {
	for name, c := range map[string]struct {
		sent, rejected, other int32
		dead, want            bool
	}{
		"nothing written, session already dead": {0, 0, 0, true, true},
		"nothing written, not dead":             {0, 0, 0, false, false},
		"own POST answered 404":                 {1, 1, 0, true, true},
		"own POST answered 404, not dead":       {1, 1, 0, false, true},
		"written, no outcome (in flight)":       {1, 0, 0, true, false},
		"written, other outcome":                {1, 0, 1, true, false},
		"404 plus another outcome":              {1, 1, 1, true, false},
		"two POSTs":                             {2, 1, 0, true, false},
	} {
		var tr callTrace
		tr.sent.Store(c.sent)
		tr.rejected.Store(c.rejected)
		tr.other.Store(c.other)
		if got := tr.mayResend(c.dead); got != c.want {
			t.Errorf("%s: %v", name, got)
		}
	}
}

func TestRemoteTransportIgnoresProxyEnvironment(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://proxy.invalid:3128")
	t.Setenv("HTTP_PROXY", "http://proxy.invalid:3128")
	tr := newRemoteTransport(true, false, nil)
	if tr.Proxy != nil {
		t.Fatal("proxy environment variables would bypass the address policy")
	}
	if tr.DialContext == nil {
		t.Fatal("no dial-time address check")
	}
}

// A server that never answers the session DELETE is released only by an explicit Abort, never by a timer.
func TestAbortReleasesStuckTeardown(t *testing.T) {
	s := newStub(t)
	stuck := make(chan struct{})
	s.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" {
			close(stuck)
			<-r.Context().Done()
			return
		}
		s.ServeHTTP(w, r)
	})
	g := s.gateway(t, nil)
	if r := request(g, "GET", "/v1/servers/r/tools", ""); r.Status != 200 {
		t.Fatal(r)
	}
	closed := make(chan struct{})
	go func() { g.Close(); close(closed) }()
	<-stuck
	select {
	case <-closed:
		t.Fatal("Close returned while the server still held the DELETE")
	case <-time.After(300 * time.Millisecond):
	}
	g.Abort()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Abort did not release the teardown")
	}
}

var _ = httptest.NewServer

// Cancellation reaches both transports: a cancelled call sends notifications/cancelled with the call's own id.
func TestCancelNotificationDeliveredLegacySSEAndStreamable(t *testing.T) {
	t.Run("streamable", func(t *testing.T) {
		s := newStub(t)
		g := s.gateway(t, nil)
		if r := request(g, "GET", "/v1/servers/r/tools", ""); r.Status != 200 {
			t.Fatal(body(r))
		}
		done := make(chan Response, 1)
		go func() {
			done <- g.Dispatch(context.Background(), Request{ID: "str1", Method: "POST", Path: "/v1/servers/r/tools/slow/call", Body: json.RawMessage(`{}`)})
		}()
		<-s.slowEntered
		request(g, "DELETE", "/v1/requests/str1", "")
		if r := <-done; r.Status != 499 {
			t.Fatal(body(r))
		}
		for deadline := time.Now().Add(5 * time.Second); s.count("notifications/cancelled") == 0; time.Sleep(10 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatal("streamable server never received notifications/cancelled")
			}
		}
	})
	t.Run("legacy-sse", func(t *testing.T) {
		out := make(chan string, 16)
		var mu sync.Mutex
		calls := map[string]string{} // method -> id of the last request with it
		cancelled := make(chan string, 1)
		entered := make(chan struct{}, 1)
		mux := http.NewServeMux()
		mux.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			w.Write([]byte("event: endpoint\ndata: /messages?s=1\n\n"))
			w.(http.Flusher).Flush()
			for {
				select {
				case m := <-out:
					w.Write([]byte("event: message\ndata: " + m + "\n\n"))
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
				Params struct {
					Name      string          `json:"name"`
					RequestID json.RawMessage `json:"requestId"`
				} `json:"params"`
			}
			json.NewDecoder(r.Body).Decode(&m)
			mu.Lock()
			calls[m.Method] = string(m.ID)
			mu.Unlock()
			w.WriteHeader(202)
			id := string(m.ID)
			switch m.Method {
			case "initialize":
				out <- `{"jsonrpc":"2.0","id":` + id + `,"result":{"protocolVersion":"2024-11-05","capabilities":{"tools":{}},"serverInfo":{"name":"s","version":"1"}}}`
			case "tools/list":
				out <- `{"jsonrpc":"2.0","id":` + id + `,"result":{"tools":[{"name":"slow","inputSchema":{"type":"object"}}]}}`
			case "tools/call":
				entered <- struct{}{}
			case "notifications/cancelled":
				cancelled <- string(m.Params.RequestID)
			}
		})
		server := httptest.NewServer(mux)
		defer server.Close()
		g, _ := New(Config{Servers: map[string]ServerConfig{"r": {URL: server.URL + "/sse", Type: "sse", ProtocolVersion: "2024-11-05"}}}, Options{})
		defer g.Close()
		if r := request(g, "GET", "/v1/servers/r/tools", ""); r.Status != 200 {
			t.Fatal(body(r))
		}
		done := make(chan Response, 1)
		go func() {
			done <- g.Dispatch(context.Background(), Request{ID: "legacy1", Method: "POST", Path: "/v1/servers/r/tools/slow/call", Body: json.RawMessage(`{}`)})
		}()
		<-entered
		request(g, "DELETE", "/v1/requests/legacy1", "")
		<-done
		select {
		case id := <-cancelled:
			mu.Lock()
			want := calls["tools/call"]
			mu.Unlock()
			if id != want {
				t.Fatalf("cancelled request %s, call was %s", id, want)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("legacy SSE server never received notifications/cancelled")
		}
	})
}

// An authentication status belongs to the call that received it; it never colours a later, unrelated failure.
func TestAuthStatusIsPerCall(t *testing.T) {
	var mode atomic.Value
	mode.Store("")
	s := newStub(t)
	inner := s.srv.Config.Handler
	s.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			b, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(b))
			if strings.Contains(string(b), `"name":"echo"`) {
				switch mode.Load().(string) {
				case "401":
					w.WriteHeader(401)
					return
				case "403":
					w.WriteHeader(403)
					return
				case "reset":
					conn, _, _ := w.(http.Hijacker).Hijack()
					conn.Close()
					return
				}
			}
		}
		inner.ServeHTTP(w, r)
	})
	g := s.gateway(t, nil)
	if r := request(g, "GET", "/v1/servers/r/tools", ""); r.Status != 200 {
		t.Fatal(body(r))
	}
	for m, want := range map[string]string{"401": "UPSTREAM_UNAUTHORIZED", "403": "UPSTREAM_FORBIDDEN"} {
		mode.Store(m)
		r := request(g, "POST", "/v1/servers/r/tools/echo/call", `{}`)
		if !strings.Contains(body(r), want) {
			t.Fatalf("%s: %s", m, body(r))
		}
	}
	mode.Store("reset")
	if r := request(g, "POST", "/v1/servers/r/tools/echo/call", `{}`); !strings.Contains(body(r), "UPSTREAM_FAILED") {
		t.Fatalf("an earlier 403 mislabelled a later transport failure: %s", body(r))
	}
}

// type:auto tries POST initialize first and falls back to /sse only on a non-auth 4xx. Asserted against a stub so the
// check does not depend on how a particular released mock answers a 2024-11-05 client.
func TestAutoFallsBackToLegacySSEOnlyOnNonAuth4xx(t *testing.T) {
	for _, status := range []int{404, 405, 401, 403} {
		out := make(chan string, 8)
		mux := http.NewServeMux()
		mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) })
		mux.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			w.Write([]byte("event: endpoint\ndata: /messages?s=1\n\n"))
			w.(http.Flusher).Flush()
			for {
				select {
				case m := <-out:
					w.Write([]byte("event: message\ndata: " + m + "\n\n"))
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
			json.NewDecoder(r.Body).Decode(&m)
			w.WriteHeader(202)
			switch m.Method {
			case "initialize":
				out <- `{"jsonrpc":"2.0","id":` + string(m.ID) + `,"result":{"protocolVersion":"2024-11-05","capabilities":{"tools":{}},"serverInfo":{"name":"s","version":"1"}}}`
			case "tools/list":
				out <- `{"jsonrpc":"2.0","id":` + string(m.ID) + `,"result":{"tools":[]}}`
			}
		})
		server := httptest.NewServer(mux)
		g, _ := New(Config{Servers: map[string]ServerConfig{"r": {URL: server.URL + "/mcp", Type: "auto"}}}, Options{})
		r := request(g, "GET", "/v1/servers/r/tools", "")
		g.Close()
		server.Close()
		fallback := status == 404 || status == 405
		if fallback && r.Status != 200 {
			t.Errorf("%d: no fallback to /sse: %s", status, body(r))
		}
		if !fallback && r.Status == 200 {
			t.Errorf("%d: an authentication failure was treated as transport discovery", status)
		}
	}
}

func TestPromptArgumentsMustBeAStringObject(t *testing.T) {
	s := newStub(t)
	g := s.gateway(t, nil)
	for _, bodyText := range []string{`{"n":1}`, `{"n":null}`, `{"n":["a"]}`, `[]`, `"x"`, `null`, `42`, ``} {
		r := request(g, "POST", "/v1/servers/r/prompts/nope", bodyText)
		if r.Status != 400 || !strings.Contains(body(r), "INVALID_ARGUMENTS") {
			t.Errorf("%q: %d %s", bodyText, r.Status, body(r))
		}
	}
	if s.count("prompts/get") != 0 {
		t.Fatal("an invalid argument object reached the server")
	}
}
