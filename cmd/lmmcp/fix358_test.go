package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func cli(args ...string) (int, string) {
	var out, errout bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	c := run(ctx, args, strings.NewReader(""), &out, &errout)
	return c, out.String()
}

// The specific configuration error is reported, not a generic INVALID_CONFIG.
func TestConfigErrorsKeepTheirCode(t *testing.T) {
	for raw, want := range map[string]string{
		`{"mcpServers":{"r":{"url":"https://example.com/mcp","headers":{"Authorization":"Bearer literal"}}}}`: "HEADER_REFERENCE_REQUIRED",
		`{"mcpServers":{"r":{"url":"https://example.com/mcp","headers":{"Mcp-Session-Id":"${X}"}}}}`:          "RESERVED_HEADER",
		`{"mcpServers":{"r":{"url":"http://example.com/mcp"}}}`:                                               "INVALID_REMOTE_URL",
		`{"mcpServers":{"r":{"url":"https://169.254.169.254/mcp"}}}`:                                          "INVALID_REMOTE_URL",
		`{"mcpServers":{"r":{"command":"x","timeout":60}}}`:                                                   "INVALID_CONFIG",
	} {
		f := filepath.Join(t.TempDir(), "c.json")
		os.WriteFile(f, []byte(raw), 0600)
		c, out := cli("servers", "--config", f)
		if c != 2 || !strings.Contains(out, want) {
			t.Errorf("%s: exit %d %s", want, c, out)
		}
	}
}

// In lmcode host mode every failure is a terminal frame with exit 2, never bare JSON.
func TestHostModeConfigFailuresAreFramed(t *testing.T) {
	dir := t.TempDir()
	program := filepath.Join(dir, "p.lmcodeb")
	os.WriteFile(program, []byte("x"), 0600)
	manifest := filepath.Join(dir, "m.json")
	os.WriteFile(manifest, []byte(`{"version":1,"mode":"single","dry":true,"parts":[{"index":0,"file":"`+program+`"}]}`), 0600)
	t.Setenv("LMCODE_INVOCATION_FILE", manifest)
	for _, cfg := range []string{filepath.Join(dir, "missing.json"), writeFile(t, dir, `{"mcpServers":{"r":{"url":"http://example.com/x"}}}`)} {
		c, out := cli("--config", cfg, "--lmcode", program)
		if c != 2 || !strings.Contains(out, "LMCODE_RETURN_BEGIN") {
			t.Errorf("exit %d, unframed output %q", c, out)
		}
	}
}
func writeFile(t *testing.T, dir, text string) string {
	f := filepath.Join(dir, "c-"+t.Name()+".json")
	os.WriteFile(f, []byte(text), 0600)
	return f
}

// The REST client never dials link-local or metadata destinations, including zone IDs, trailing dots, and alternate IPv4 encodings.
func TestClientRefusesMetadataDestinations(t *testing.T) {
	for _, u := range []string{
		"https://169.254.169.254",
		"https://169.254.169.254.",
		"https://2852039166",
		"https://0251.0376.0251.0376",
		"https://0xa9fea9fe",
		"https://[fe80::1]",
		"https://[fe80::1%25lo]",
		"https://[fe80::1%25eth0]",
		"https://0.0.0.0",
		"https://[::%25lo]",
		"https://[fd00:ec2::254]",
		"https://[fd00:ec2::254%25lo]",
		"https://[::ffff:169.254.169.254%25lo]",
		"http://2130706433:8080",
		"http://0x7f.0.0.1:8080",
		"http://127.0.0.1.:8080",
	} {
		if c, out := cli("servers", "--url", u); c != 2 || !strings.Contains(out, "CLIENT_CONFIG") {
			t.Errorf("%s: exit %d %s", u, c, out)
		}
	}
}

func TestSecondSignalHelperProcess(t *testing.T) {
	if os.Getenv("LMMCP_TEST_SECOND_SIGNAL") != "1" {
		return
	}
	for i, a := range os.Args {
		if a == "--" {
			os.Args = append([]string{os.Args[0]}, os.Args[i+1:]...)
			break
		}
	}
	main()
}

// P1-3: A second SIGINT/SIGTERM must force exit even if the remote server never answers the session DELETE during g.Close().
func TestSecondSignalAbortsStuckDeleteOnShutdown(t *testing.T) {
	deleteEntered := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "GET":
			w.WriteHeader(405)
		case "DELETE":
			select {
			case <-deleteEntered:
			default:
				close(deleteEntered)
			}
			<-r.Context().Done()
		case "POST":
			var m struct {
				ID     any    `json:"id"`
				Method string `json:"method"`
			}
			json.NewDecoder(r.Body).Decode(&m)
			w.Header().Set("Content-Type", "application/json")
			if m.Method == "initialize" {
				w.Header().Set("Mcp-Session-Id", "S-stuck")
				json.NewEncoder(w).Encode(map[string]any{
					"jsonrpc": "2.0", "id": m.ID,
					"result": map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "s", "version": "1"}},
				})
				return
			}
			if m.ID == nil {
				w.WriteHeader(202)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{"tools": []any{}}})
		}
	}))

	cfg := filepath.Join(t.TempDir(), "c.json")
	os.WriteFile(cfg, []byte(`{"mcpServers":{"r":{"url":"`+srv.URL+`","protocolVersion":"2025-11-25"}}}`), 0600)

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestSecondSignalHelperProcess$", "--", "tools", "r", "--config", cfg)
	cmd.Env = append(os.Environ(), "LMMCP_TEST_SECOND_SIGNAL=1")
	if err := cmd.Start(); err != nil {
		srv.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
		srv.CloseClientConnections()
		srv.Close()
	})

	select {
	case <-deleteEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("server never received session DELETE")
	}

	// While g.Close() is blocked in DELETE, send SIGINT (and a second SIGTERM) to abort teardown.
	_ = cmd.Process.Signal(syscall.SIGINT)
	time.Sleep(30 * time.Millisecond)
	_ = cmd.Process.Signal(syscall.SIGTERM)

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("process hung in g.Close() after second signal while server held DELETE")
	}
}

// The --url REST client uses SafeDialer at dial time so a hostname resolving to 0.0.0.0 (which Linux routes to loopback) is refused before connecting.
func TestClientSafeDialerRefusesForbiddenResolvedIP(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var accepted atomic.Int64
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			_ = c.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())

	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
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
				resp = append(resp, 0xc0, 0x0c, 0, 1, 0, 1, 0, 0, 0, 60, 0, 4, 0, 0, 0, 0)
			} else {
				resp[7] = 0
				resp[3] = 0x80
			}
			pc.WriteTo(resp, addr)
		}
	}()
	prev := net.DefaultResolver
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "udp4", pc.LocalAddr().String())
	}}
	defer func() { net.DefaultResolver = prev }()

	c, _ := cli("servers", "--url", "https://zero.test:"+port)
	if c == 0 || accepted.Load() != 0 {
		t.Fatalf("client connected TCP to 0.0.0.0 after DNS resolution (exit=%d accepted=%d)", c, accepted.Load())
	}
}
