//go:build integration

package bridge

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func releasedMock(t *testing.T, version, mode, token string) string {
	t.Helper()
	binary, program := os.Getenv("LMMCP_TEST_LMMOCK"), os.Getenv("LMMCP_TEST_PROGRAM")
	if binary == "" || program == "" {
		t.Fatal("integration requires released lmmock binary and compiled MCP program")
	}
	cfg := filepath.Join(t.TempDir(), "mock.json")
	b, _ := json.Marshal(map[string]string{"protocolVersion": version, "replyMode": mode, "bearerToken": token})
	os.WriteFile(cfg, b, 0600)
	cmd := exec.Command(binary, "serve", "--lmcode", program, "--config", cfg)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	out, e := cmd.StdoutPipe()
	if e != nil {
		t.Fatal(e)
	}
	cmd.Stderr = io.Discard
	if e = cmd.Start(); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = cmd.Process.Signal(os.Interrupt); _ = cmd.Wait() })
	var ready struct {
		URL string `json:"url"`
	}
	if e = json.NewDecoder(out).Decode(&ready); e != nil {
		t.Fatal(e)
	}
	controlAuth(t, ready.URL, "/__mock/reply-mode", `{"mode":"`+mode+`"}`, token)
	return ready.URL
}
func control(t *testing.T, base, path, body string) map[string]any {
	return controlAuth(t, base, path, body, "")
}

// controlAuth sends the mock's own bearer token when it has one: v0.1.111 guards /__mock/* like every other route,
// while v0.1.110 (wp353 F2) answered them without any credential.
func controlAuth(t *testing.T, base, path, body, token string) map[string]any {
	t.Helper()
	req, _ := http.NewRequest("POST", base+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, e := http.DefaultClient.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatal(res.StatusCode)
	}
	var data map[string]any
	if e = json.NewDecoder(res.Body).Decode(&data); e != nil {
		t.Fatal(e)
	}
	return data
}
func TestReleasedMatrix(t *testing.T) {
	for _, version := range []string{"2026-07-28", "2025-11-25", "2024-11-05"} {
		for _, mode := range []string{"json", "sse"} {
			t.Run(version+"/"+mode, func(t *testing.T) {
				base := releasedMock(t, version, mode, "")
				kind, path := "http", "/mcp"
				if version == "2024-11-05" {
					kind, path = "sse", "/sse"
				}
				g, _ := New(Config{Servers: map[string]ServerConfig{"r": {URL: base + path, Type: kind, ProtocolVersion: version}}}, Options{})
				defer g.Close()
				for _, path := range []string{"tools", "resources", "resources/templates", "prompts"} {
					r := request(g, "GET", "/v1/servers/r/"+path, "")
					if r.Status != 200 {
						t.Fatal(path, r)
					}
				}
				for _, tool := range []struct{ name, args, want string }{{"echo", `{"text":"hello"}`, structuredWant(version, `"structuredContent":{"text":"hello"}`)}, {"add", `{"left":2,"right":3}`, structuredWant(version, `"structuredContent":{"sum":5}`)}, {"fail", `{}`, `"isError":true`}} {
					r := request(g, "POST", "/v1/servers/r/tools/"+tool.name+"/call", tool.args)
					b, _ := json.Marshal(r.Body)
					if r.Status != 200 || !strings.Contains(string(b), tool.want) {
						t.Fatal(tool.name, r, string(b))
					}
					if tool.name == "fail" && Exit(r) != 3 {
						t.Fatal("tool exit")
					}
				}
				for _, uri := range []string{"mock://text", "mock://blob"} {
					r := g.Dispatch(context.Background(), Request{Method: "GET", Path: "/v1/servers/r/resources/read", Query: map[string]string{"uri": uri}})
					b, _ := json.Marshal(r.Body)
					if r.Status != 200 || uri == "mock://blob" && !strings.Contains(string(b), `"blob":"AAEC"`) {
						t.Fatal(r, string(b))
					}
				}
				r := request(g, "POST", "/v1/servers/r/prompts/greet", `{"name":"Ada"}`)
				b, _ := json.Marshal(r.Body)
				if r.Status != 200 || !strings.Contains(string(b), "Hello Ada") {
					t.Fatal(r, string(b))
				}
				if r = request(g, "POST", "/v1/servers/r/tools/crash/call", `{}`); r.Status != 502 {
					t.Fatal(r)
				}
			})
		}
	}
}
func TestReleasedCancelAndChanged(t *testing.T) {
	for _, version := range []string{"2026-07-28", "2025-11-25", "2024-11-05"} {
		t.Run(version, func(t *testing.T) {
			base := releasedMock(t, version, "sse", "")
			kind, path := "http", "/mcp"
			if version == "2024-11-05" {
				kind, path = "sse", "/sse"
			}
			events := make(chan Event, 100)
			g, _ := New(Config{Servers: map[string]ServerConfig{"r": {URL: base + path, Type: kind, ProtocolVersion: version}}}, Options{Event: func(e Event) { events <- e }})
			defer g.Close()
			if r := request(g, "GET", "/v1/servers/r/tools", ""); r.Status != 200 {
				t.Fatal(r)
			}
			previous := g.entries["r"].session
			control(t, base, "/__mock/list-changed", `{}`)
			for e := range events {
				if e.Kind == "list_changed" {
					break
				}
			}
			if r := request(g, "GET", "/v1/servers/r/tools", ""); r.Status != 200 || g.entries["r"].session == previous {
				t.Fatal("list change did not refresh session", r)
			}
			done := make(chan Response, 1)
			go func() {
				done <- g.Dispatch(context.Background(), Request{ID: "slow", Method: "POST", Path: "/v1/servers/r/tools/slow/call", Body: json.RawMessage(`{}`)})
			}()
			progressReady := false
			for !progressReady {
				select {
				case event := <-events:
					progressReady = event.Kind == "progress"
				case response := <-done:
					t.Fatalf("slow completed before progress: %#v", response)
				}
			}
			if r := request(g, "DELETE", "/v1/requests/slow", ""); r.Status != 200 {
				t.Fatal(r)
			}
			if r := <-done; r.Status != 499 {
				t.Fatal(r)
			}
			// The mock acknowledges a cancellation in its state. Bounded harness wait, not a product deadline.
			acknowledged := false
			var last map[string]any
			for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline) && !acknowledged; time.Sleep(20 * time.Millisecond) {
				stateResponse, e := http.Get(base + "/__mock/state")
				if e != nil {
					t.Fatal(e)
				}
				e = json.NewDecoder(stateResponse.Body).Decode(&last)
				stateResponse.Body.Close()
				if e != nil {
					t.Fatal(e)
				}
				n, ok := last["cancelledSlow"].(float64)
				acknowledged = ok && n >= 1
			}
			// The released mock's own cancel counter changed between v0.1.110 (a dropped connection also counts) and
			// v0.1.111 (only a notification whose requestId matches its key counts). Delivery of notifications/cancelled
			// is asserted against conforming stubs in fix358_test.go (streamable and legacy SSE), so the counter is
			// required only for 2026-07-28, where closing the stream is the cancellation.
			if !acknowledged && version == "2026-07-28" {
				t.Fatalf("the mock never acknowledged the cancellation: %v", last)
			}
			if !acknowledged {
				t.Logf("mock cancel counter stayed 0 for %s: %v", version, last)
			}
			if r := request(g, "GET", "/v1/servers/r/tools", ""); r.Status != 200 {
				t.Fatal(r)
			}
		})
	}
}
func TestReleasedExpiry(t *testing.T) {
	base := releasedMock(t, "2025-11-25", "json", "")
	g, _ := New(Config{Servers: map[string]ServerConfig{"r": {URL: base + "/mcp", ProtocolVersion: "2025-11-25"}}}, Options{})
	defer g.Close()
	if r := request(g, "GET", "/v1/servers/r/tools", ""); r.Status != 200 {
		t.Fatal(r)
	}
	e := g.entries["r"]
	sid := e.session.ID()
	b, _ := json.Marshal(map[string]string{"sessionId": sid})
	control(t, base, "/__mock/expire-session", string(b))
	if r := request(g, "GET", "/v1/servers/r/tools", ""); r.Status != 200 {
		t.Fatal(r)
	}
	if e.session.ID() == sid {
		t.Fatal("session not replaced")
	}
}
func TestReleasedAutoAndAuth(t *testing.T) {
	base := releasedMock(t, "2024-11-05", "json", "")
	g, _ := New(Config{Servers: map[string]ServerConfig{"r": {URL: base + "/mcp", Type: "auto"}}}, Options{})
	if r := request(g, "GET", "/v1/servers/r/tools", ""); r.Status != 200 {
		t.Fatal(r)
	}
	g.Close()
	base = releasedMock(t, "2026-07-28", "json", "synthetic-354")
	for _, entry := range []struct {
		headers map[string]string
		code    string
	}{{nil, "UPSTREAM_UNAUTHORIZED"}, {map[string]string{"Authorization": "Bearer ${KEY}"}, ""}, {map[string]string{"Authorization": "Bearer ${KEY}", "Host": "${BAD_HOST}"}, "UPSTREAM_FORBIDDEN"}, {map[string]string{"Authorization": "Bearer ${KEY}", "Origin": "${BAD_ORIGIN}"}, "UPSTREAM_FORBIDDEN"}} {
		g, _ := New(Config{Servers: map[string]ServerConfig{"r": {URL: base + "/mcp", Headers: entry.headers}}}, Options{Lookup: func(s string) (string, bool) {
			switch s {
			case "KEY":
				return "synthetic-354", true
			case "BAD_HOST":
				return "evil.invalid", true
			case "BAD_ORIGIN":
				return "https://evil.invalid", true
			}
			return "", false
		}})
		r := request(g, "GET", "/v1/servers/r/tools", "")
		b, _ := json.Marshal(r.Body)
		if entry.code == "" && r.Status != 200 || entry.code != "" && !strings.Contains(string(b), entry.code) {
			t.Fatal(r, string(b))
		}
		g.Close()
	}
}

// structuredWant: revision 2024-11-05 has no structuredContent; the released mock stopped emitting it there in
// v0.1.111, so only the text content is expected for that revision.
func structuredWant(version, want string) string {
	if version == "2024-11-05" {
		return `"type":"text"`
	}
	return want
}
