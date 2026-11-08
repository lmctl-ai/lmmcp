package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
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
