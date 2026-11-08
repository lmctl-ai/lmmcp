package bridge

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// P1-1: an effectful call is never re-sent. One call's 404 must not make a different in-flight call replay.
func TestEffectCallNeverResent(t *testing.T) {
	s := newStub(t)
	s.holdFirst = true
	g := s.gateway(t, nil)
	if r := request(g, "GET", "/v1/servers/r/tools", ""); r.Status != 200 {
		t.Fatal(r)
	}
	s.mu.Lock()
	s.expireTool = "echo"
	s.mu.Unlock()
	aDone := make(chan Response, 1)
	go func() { aDone <- request(g, "POST", "/v1/servers/r/tools/effect/call", `{}`) }()
	<-s.entered
	b := request(g, "POST", "/v1/servers/r/tools/echo/call", `{}`)
	if b.Status != 200 {
		t.Fatalf("the call whose own POST got the 404 must be retried once on a new session: %s", body(b))
	}
	close(s.holdRelease)
	<-aDone
	if n := s.effectCount(); n != 1 {
		t.Fatalf("effect executed %d times", n)
	}
}

// P1-1: no failure path may re-send a call whose request was written; only that call's own 404 allows a retry.
func TestNoResendAfterWrittenRequest(t *testing.T) {
	for _, mode := range []string{"reset-after-execute", "404-after-execute"} {
		t.Run(mode, func(t *testing.T) {
			var effects atomic.Int64
			var sessions sync.Map
			var inits atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" {
					w.WriteHeader(405)
					return
				}
				var m struct {
					ID     any    `json:"id"`
					Method string `json:"method"`
					Params struct {
						Name string `json:"name"`
					} `json:"params"`
				}
				json.NewDecoder(r.Body).Decode(&m)
				w.Header().Set("Content-Type", "application/json")
				write := func(v any) {
					b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": v})
					w.Write(b)
				}
				switch {
				case m.Method == "initialize":
					id := "S" + string(rune('0'+inits.Add(1)))
					sessions.Store(id, true)
					if mode != "404-after-execute" {
						w.Header().Set("Mcp-Session-Id", id)
					}
					write(map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "x", "version": "1"}})
				case m.ID == nil:
					w.WriteHeader(202)
				case m.Method == "tools/list":
					write(map[string]any{"tools": []any{map[string]any{"name": "effect", "inputSchema": map[string]any{"type": "object"}}}})
				case m.Method == "tools/call":
					effects.Add(1) // the server executes, then the answer is lost
					if mode == "reset-after-execute" {
						conn, _, _ := w.(http.Hijacker).Hijack()
						conn.Close()
						return
					}
					sessions.Delete(r.Header.Get("Mcp-Session-Id"))
					w.WriteHeader(404) // executed, yet answered 404: still not safe to repeat only if the body was written
				}
			}))
			defer server.Close()
			g, _ := New(Config{Servers: map[string]ServerConfig{"r": {URL: server.URL, ProtocolVersion: "2025-11-25"}}}, Options{})
			defer g.Close()
			r := request(g, "POST", "/v1/servers/r/tools/effect/call", `{}`)
			if r.Status == 200 {
				t.Fatalf("unexpected success: %s", body(r))
			}
			if effects.Load() != 1 {
				t.Fatalf("%s: effect ran %d times", mode, effects.Load())
			}
		})
	}
}

// P1-2: an application error from an unrelated request must not abort a running call or restart the session.
func TestUnrelatedErrorKeepsRunningCall(t *testing.T) {
	s := newStub(t)
	g := s.gateway(t, nil)
	if r := request(g, "GET", "/v1/servers/r/tools", ""); r.Status != 200 {
		t.Fatal(r)
	}
	done := make(chan Response, 1)
	go func() { done <- request(g, "POST", "/v1/servers/r/tools/slow/call", `{}`) }()
	<-s.slowEntered
	for _, path := range []string{"/v1/servers/r/prompts/nope", "/v1/servers/r/prompts/boom", "/v1/servers/r/prompts/other"} {
		request(g, "POST", path, `{}`)
	}
	request(g, "GET", "/v1/servers/r/resources/read?uri=mock%3A%2F%2Fmissing", "")
	select {
	case r := <-done:
		t.Fatalf("running call was aborted by unrelated errors: %s", body(r))
	case <-time.After(500 * time.Millisecond):
	}
	close(s.slowRelease)
	if r := <-done; r.Status != 200 || !strings.Contains(body(r), "slow done") {
		t.Fatalf("slow call did not complete: %s", body(r))
	}
	if n := s.count("initialize"); n != 1 {
		t.Fatalf("session re-initialized %d times", n)
	}
}

func TestUnrelatedErrorKeepsStdioChild(t *testing.T) {
	g, events := fixture(t, false)
	done := make(chan Response, 1)
	go func() {
		done <- g.Dispatch(context.Background(), Request{ID: "slowstdio", Method: "POST", Path: "/v1/servers/mock/tools/slow/call", Body: json.RawMessage(`{}`)})
	}()
	for progress := false; !progress; {
		select {
		case e := <-events:
			progress = e.Kind == "progress"
		case x := <-done:
			t.Fatalf("slow call ended before progress: %s", body(x))
		case <-time.After(20 * time.Second):
			t.Fatal("no progress event from the stdio server")
		}
	}
	started := g.StartCount()
	bad := make(chan Response, 1)
	go func() {
		bad <- g.Dispatch(context.Background(), Request{Method: "GET", Path: "/v1/servers/mock/resources/read", Query: map[string]string{"uri": "mock://missing"}})
	}()
	select {
	case r := <-bad:
		if r.Status < 400 {
			t.Fatalf("missing resource should fail: %s", body(r))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the failing request never returned (teardown waited on the busy child)")
	}
	select {
	case x := <-done:
		t.Fatalf("running stdio call died with an unrelated error: %s", body(x))
	case <-time.After(time.Second):
	}
	if g.StartCount() != started {
		t.Fatal("child restarted")
	}
	request(g, "DELETE", "/v1/requests/slowstdio", "")
	select {
	case x := <-done:
		if x.Status != 499 {
			t.Fatal(body(x))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancelled call never returned")
	}
	if g.StartCount() != started {
		t.Fatal("child restarted after the cancelled call")
	}
}

// P2-3: the session DELETE reaches the server on shutdown.
func TestSessionDeleteReachesServerOnClose(t *testing.T) {
	s := newStub(t)
	g := s.gateway(t, nil)
	if r := request(g, "GET", "/v1/servers/r/tools", ""); r.Status != 200 {
		t.Fatal(r)
	}
	g.Close()
	if s.count("DELETE") != 1 {
		t.Fatalf("server saw %d DELETE requests", s.count("DELETE"))
	}
}

// P2-2: hostile notification streams use constant memory, and list_changed is still recognised.
func TestNotificationStreamFlatMemory(t *testing.T) {
	for _, shape := range []string{"endless-data-lines", "endless-line"} {
		t.Run(shape, func(t *testing.T) {
			s := newStub(t)
			chunk := []byte("data: " + strings.Repeat("A", 1<<20) + "\n")
			streamed := make(chan struct{})
			s.onGET = func(w http.ResponseWriter, r *http.Request) {
				defer close(streamed)
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(200)
				w.(http.Flusher).Flush()
				for i := 0; i < 300; i++ {
					if shape == "endless-line" {
						w.Write(chunk[6 : len(chunk)-1])
					} else {
						w.Write(chunk)
					}
					w.(http.Flusher).Flush()
					if r.Context().Err() != nil {
						return
					}
				}
			}
			g := s.gateway(t, nil)
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
				t.Fatal(r)
			}
			<-streamed
			close(stop)
			if grown := int64(peak.Load()) - int64(base.HeapAlloc); grown > 96<<20 {
				t.Fatalf("heap grew by %d MiB while the stream carried 300 MiB", grown>>20)
			}
		})
	}
}

func TestNotificationStreamStillSeesListChanged(t *testing.T) {
	for _, eol := range []string{"\n", "\r\n"} {
		s := newStub(t)
		s.onGET = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			w.Write([]byte("event: message" + eol + "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/other\",\"params\":{\"x\":\"" + strings.Repeat("z", 200000) + "\"}}" + eol + eol))
			w.Write([]byte("data: {\"jsonrpc\":\"2.0\"," + eol + "data: \"method\":\"notifications/tools/list_changed\"}" + eol + eol))
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}
		events := make(chan Event, 10)
		g, _ := New(Config{Servers: map[string]ServerConfig{"r": {URL: s.srv.URL + "/mcp", ProtocolVersion: "2025-11-25"}}}, Options{Event: func(e Event) { events <- e }})
		defer g.Close()
		if r := request(g, "GET", "/v1/servers/r/tools", ""); r.Status != 200 {
			t.Fatal(r)
		}
		for e := range events {
			if e.Kind == "list_changed" {
				break
			}
		}
	}
}

// P2-4: JSON-RPC errors map to typed REST errors with their MCP code; upstream text is never echoed.
func TestJSONRPCErrorsMapToRESTErrors(t *testing.T) {
	s := newStub(t)
	g := s.gateway(t, nil)
	for _, c := range []struct {
		method, path string
		status       int
		code         string
		mcp          int
	}{
		{"POST", "/v1/servers/r/prompts/nope", 404, "PROMPT_NOT_FOUND", -32602},
		{"POST", "/v1/servers/r/prompts/bad", 400, "INVALID_PARAMS", -32602},
		{"POST", "/v1/servers/r/prompts/boom", 502, "UPSTREAM_INTERNAL_ERROR", -32603},
		{"POST", "/v1/servers/r/prompts/other", 502, "UPSTREAM_ERROR", -32000},
		{"GET", "/v1/servers/r/resources/read?uri=mock%3A%2F%2Fmissing", 404, "RESOURCE_NOT_FOUND", -32002},
	} {
		var r Response
		if c.method == "GET" {
			r = g.Dispatch(context.Background(), Request{Method: "GET", Path: "/v1/servers/r/resources/read", Query: map[string]string{"uri": "mock://missing"}})
		} else {
			r = request(g, c.method, c.path, `{}`)
		}
		b := body(r)
		var out struct {
			Error struct {
				Code    string `json:"code"`
				MCPCode int    `json:"mcpCode"`
			} `json:"error"`
		}
		json.Unmarshal([]byte(b), &out)
		if r.Status != c.status || out.Error.Code != c.code || out.Error.MCPCode != c.mcp || strings.Contains(b, "SECRET-TEXT-FROM-UPSTREAM") {
			t.Fatalf("%s: status %d body %s", c.path, r.Status, b)
		}
		wantExit := 2
		if c.status >= 500 {
			wantExit = 3
		}
		if Exit(r) != wantExit {
			t.Fatalf("%s: exit %d", c.path, Exit(r))
		}
	}
	if s.count("initialize") != 1 {
		t.Fatal("application errors must not re-initialize the session")
	}
}

// P2-1: address policy.
func TestNetworkPolicyLiterals(t *testing.T) {
	refuse := []string{"https://169.254.169.254/latest", "https://169.254.0.1/x", "https://[fe80::1]/x", "https://[fd00:ec2::254]/x", "https://0.0.0.0/x", "https://[::]/x", "https://[::ffff:169.254.169.254]/x", "https://example.com:0/mcp", "https://example.com:99999/mcp", "https://example.com/mcp%00", "https://example.com/mcp?"}
	allow := []string{"https://10.0.0.5/mcp", "https://192.168.1.1/mcp", "https://[fd00::1]/mcp", "https://127.0.0.1/mcp", "https://localhost/mcp", "https://example.com/mcp", "https://example.com:8443/mcp", "http://127.0.0.1:8080/mcp", "http://[::1]:8080/mcp"}
	for _, u := range refuse {
		if _, e := Load([]byte(`{"mcpServers":{"r":{"url":"` + u + `"}}}`)); e == nil {
			t.Errorf("accepted %s", u)
		}
	}
	for _, u := range allow {
		if _, e := Load([]byte(`{"mcpServers":{"r":{"url":"` + u + `"}}}`)); e != nil {
			t.Errorf("refused %s: %v", u, e)
		}
	}
}

func TestAllowPrivateAddressesFlag(t *testing.T) {
	cfg := func(u, flag string) error {
		_, e := Load([]byte(`{"mcpServers":{"r":{"url":"` + u + `"` + flag + `}}}`))
		return e
	}
	if e := cfg("https://10.0.0.5/mcp", `,"allowPrivateAddresses":true`); e != nil {
		t.Fatal(e)
	}
	if e := cfg("https://10.0.0.5/mcp", `,"allowPrivateAddresses":false`); e == nil {
		t.Fatal("private literal accepted with the flag off")
	}
	if e := cfg("https://[fd00::1]/mcp", `,"allowPrivateAddresses":false`); e == nil {
		t.Fatal("ULA literal accepted with the flag off")
	}
	if e := cfg("https://example.com/mcp", `,"allowPrivateAddresses":false`); e != nil {
		t.Fatal(e)
	}
	if e := cfg("http://127.0.0.1:8080/mcp", `,"allowPrivateAddresses":false`); e != nil {
		t.Fatalf("literal-loopback http is the documented local mode: %v", e)
	}
	if _, e := Load([]byte(`{"mcpServers":{"a":{"command":"x","allowPrivateAddresses":false}}}`)); e == nil {
		t.Fatal("flag on a stdio server")
	}
}

// stubDNS answers every A query with one address (UDP), to exercise dial-time checks.
func stubDNS(t *testing.T, ip net.IP) func() {
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		buf := make([]byte, 512)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			q := buf[:n]
			end := 12
			for end < n && q[end] != 0 {
				end += int(q[end]) + 1
			}
			question := q[12 : end+5]
			resp := append([]byte{q[0], q[1], 0x81, 0x80, 0, 1, 0, 1, 0, 0, 0, 0}, question...)
			if q[end+2] == 1 {
				resp = append(resp, 0xc0, 0x0c, 0, 1, 0, 1, 0, 0, 0, 60, 0, 4)
				resp = append(resp, ip.To4()...)
			} else {
				resp[7] = 0 // no answer for other types
				resp[3] = 0x80
			}
			pc.WriteTo(resp, addr)
		}
	}()
	previous := net.DefaultResolver
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "udp4", pc.LocalAddr().String())
	}}
	return func() { net.DefaultResolver = previous; pc.Close() }
}

func TestDialTimeRefusesMetadataAfterDNS(t *testing.T) {
	defer stubDNS(t, net.ParseIP("169.254.169.254"))()
	g, _ := New(Config{Servers: map[string]ServerConfig{"r": {URL: "https://meta.test:8443/mcp"}}}, Options{})
	defer g.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	r := g.Dispatch(ctx, Request{Method: "GET", Path: "/v1/servers/r/tools"})
	if !strings.Contains(body(r), "ADDRESS_REFUSED") {
		t.Fatalf("a name resolving to cloud metadata was dialled: %s", body(r))
	}
}

func TestDialTimePrivateAllowedByDefaultAndTightenable(t *testing.T) {
	tls := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer tls.Close()
	_, port, _ := net.SplitHostPort(tls.Listener.Addr().String())
	defer stubDNS(t, net.ParseIP("127.0.0.1"))()
	for _, c := range []struct {
		flag    string
		refused bool
	}{{``, false}, {`,"allowPrivateAddresses":true`, false}, {`,"allowPrivateAddresses":false`, true}} {
		cfg, e := Load([]byte(`{"mcpServers":{"r":{"url":"https://corp.test:` + port + `/mcp"` + c.flag + `}}}`))
		if e != nil {
			t.Fatal(e)
		}
		g, _ := New(cfg, Options{})
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		r := g.Dispatch(ctx, Request{Method: "GET", Path: "/v1/servers/r/tools"})
		cancel()
		g.Close()
		if got := strings.Contains(body(r), "ADDRESS_REFUSED"); got != c.refused {
			t.Fatalf("flag %q: refused=%v want %v: %s", c.flag, got, c.refused, body(r))
		}
	}
}

// P2-5 mutation killers.
func TestRemoteHeaderAndURLPolicyDetails(t *testing.T) {
	for name, raw := range map[string]string{
		"reserved session id": `{"url":"http://127.0.0.1/mcp","headers":{"Mcp-Session-Id":"${X}"}}`,
		"reserved protocol":   `{"url":"http://127.0.0.1/mcp","headers":{"MCP-Protocol-Version":"${X}"}}`,
		"duplicate by case":   `{"url":"http://127.0.0.1/mcp","headers":{"Authorization":"${X}","authorization":"${Y}"}}`,
		"url query":           `{"url":"http://127.0.0.1/mcp?x=1"}`,
		"url userinfo":        `{"url":"http://u:p@127.0.0.1/mcp"}`,
		"url fragment":        `{"url":"http://127.0.0.1/mcp#f"}`,
	} {
		if _, e := Load([]byte(`{"mcpServers":{"r":` + raw + `}}`)); e == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func originState(t *testing.T, origin string) *remoteState {
	u, _ := url.Parse(origin)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &remoteState{ctx: ctx, cancel: cancel, origin: u, headers: http.Header{"Authorization": []string{"Bearer synthetic-358"}}, base: &http.Transport{}}
}

func TestRoundTripSchemeUserinfoAndDefaultPort(t *testing.T) {
	var hits atomic.Int64
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer plain.Close()
	host := strings.TrimPrefix(plain.URL, "http://")
	// https origin, request downgraded to http on the same host:port
	state := originState(t, "https://"+host)
	req, _ := http.NewRequest("GET", plain.URL+"/x", nil)
	if r, e := state.RoundTrip(req); e == nil {
		r.Body.Close()
		t.Fatal("scheme downgrade accepted")
	}
	// userinfo in a same-origin URL
	state = originState(t, plain.URL)
	req, _ = http.NewRequest("GET", "http://u:p@"+host+"/x", nil)
	if r, e := state.RoundTrip(req); e == nil {
		r.Body.Close()
		t.Fatal("userinfo URL accepted")
	}
	if hits.Load() != 0 {
		t.Fatal("request reached the server")
	}
	// the default port is the same origin
	state = originState(t, "http://127.0.0.1:80")
	req, _ = http.NewRequest("GET", "http://127.0.0.1/x", nil)
	if _, e := state.RoundTrip(req); e != nil && strings.Contains(e.Error(), "REMOTE_ORIGIN_CHANGED") {
		t.Fatal("explicit default port treated as another origin")
	}
	// a non-default explicit port must not match an implicit default port
	state = originState(t, "http://127.0.0.1:8080")
	req, _ = http.NewRequest("GET", "http://127.0.0.1/x", nil)
	if r, e := state.RoundTrip(req); e == nil {
		r.Body.Close()
		t.Fatal("non-default port matched implicit default port")
	}
}

func TestSameOriginRedirectIsRefused(t *testing.T) {
	for _, status := range []int{301, 302, 307, 308} {
		var followed atomic.Int64
		code := status
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/other" {
				followed.Add(1)
				w.WriteHeader(500)
				return
			}
			http.Redirect(w, r, "/other", code)
		}))
		g, _ := New(Config{Servers: map[string]ServerConfig{"r": {URL: server.URL + "/mcp", ProtocolVersion: "2025-11-25"}}}, Options{})
		if r := request(g, "GET", "/v1/servers/r/tools", ""); r.Status < 400 {
			t.Fatalf("status %d: %v", code, r)
		}
		g.Close()
		server.Close()
		if followed.Load() != 0 {
			t.Fatalf("status %d: a same-origin redirect was followed", code)
		}
	}
}

func TestRemoteBearerNeverInResults(t *testing.T) {
	s := newStub(t)
	const token = "synthetic-bearer-358-abcdef"
	g, _ := New(Config{Servers: map[string]ServerConfig{"r": {URL: s.srv.URL + "/mcp", ProtocolVersion: "2025-11-25", Headers: map[string]string{"Authorization": "Bearer ${TOK}"}}}}, Options{Lookup: func(n string) (string, bool) { return token, n == "TOK" }})
	defer g.Close()
	r := request(g, "POST", "/v1/servers/r/tools/leak/call", `{}`)
	if strings.Contains(body(r), token) || r.Status != 502 || !strings.Contains(body(r), "SECRET_OUTPUT") {
		t.Fatalf("remote bearer echoed by a tool was not suppressed: %d %s", r.Status, body(r))
	}
}

func TestProtectScansKeysAndPaginationCyclesAreRefused(t *testing.T) {
	const secret = "synthetic-secret-358"
	r := Protect(success(map[string]any{secret: 1}), []string{secret})
	if !strings.Contains(body(r), "SECRET_OUTPUT") {
		t.Fatal("secret used as an object key was returned")
	}
	calls := 0
	_, e := pages(func(cursor string) ([]int, string, error) {
		if calls++; calls > 50 {
			return nil, "", bad("TEST_CYCLE_NOT_DETECTED")
		}
		return []int{1}, "same", nil
	})
	if e == nil || e.Error() != "INVALID_PAGINATION" {
		t.Fatalf("cursor cycle: %v", e)
	}
}

var _ = sync.Mutex{}
