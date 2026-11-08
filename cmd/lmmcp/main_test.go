package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/lmctlhq/lmmcp/internal/bridge"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestCLI(t *testing.T) {
	for _, args := range [][]string{{"openapi"}, {"--version"}} {
		var out, err bytes.Buffer
		if c := run(context.Background(), args, strings.NewReader(""), &out, &err); c != 0 || !json.Valid(out.Bytes()) {
			t.Fatal(c, out.String(), err.String())
		}
	}
	for _, args := range [][]string{{"call"}, {"serve", "--listen", "0.0.0.0:0", "--config", "missing"}, {"servers", "--token", "SYNTHETIC_SECRET_350"}, {"call", "s", "t", "--arg", "x=1", "--arg", "x=2"}} {
		var out, err bytes.Buffer
		if c := run(context.Background(), args, strings.NewReader(""), &out, &err); c != 2 || !json.Valid(out.Bytes()) || strings.Contains(out.String()+err.String(), "SYNTHETIC_SECRET_350") {
			t.Fatal(c, out.String(), err.String())
		}
	}
}
func TestURLClientAndNoRedirect(t *testing.T) {
	g, _ := bridge.New(bridge.Config{Servers: map[string]bridge.ServerConfig{}}, bridge.Options{})
	defer g.Close()
	srv := httptest.NewServer(g.Handler("", ""))
	defer srv.Close()
	var out, err bytes.Buffer
	if c := run(context.Background(), []string{"servers", "--url", srv.URL}, strings.NewReader(""), &out, &err); c != 0 || !strings.Contains(out.String(), `"ok":true`) {
		t.Fatal(c, out.String(), err.String())
	}
}
func TestClientCredentialReflectionAndInvalidURL(t *testing.T) {
	const token = "SYNTHETIC_REFLECTION_TOKEN_350"
	t.Setenv("LMMCP_TEST_AUTH", token)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": map[string]string{"echo": token}})
	}))
	defer srv.Close()
	var out, err bytes.Buffer
	c := run(context.Background(), []string{"servers", "--url", srv.URL, "--token-env", "LMMCP_TEST_AUTH"}, strings.NewReader(""), &out, &err)
	if c != 3 || strings.Contains(out.String()+err.String(), token) {
		t.Fatal(c, out.String(), err.String())
	}
	out.Reset()
	if c := run(context.Background(), []string{"servers", "--url", "http://example.invalid"}, strings.NewReader(""), &out, &err); c != 2 {
		t.Fatal(c)
	}
}
func TestRedirectNeverForwardsToken(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("redirect followed") }))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 302) }))
	defer srv.Close()
	var out, err bytes.Buffer
	if c := run(context.Background(), []string{"servers", "--url", srv.URL}, strings.NewReader(""), &out, &err); c != 3 {
		t.Fatal(c)
	}
}
func TestCLIStrictFlagValidation(t *testing.T) {
	for _, args := range [][]string{
		{"openapi", "extra"},
		{"openapi", "--url", "http://127.0.0.1:8080"},
		{"openapi", "--config", "cfg.json"},
		{"servers", "--config", "a.json", "--config", "b.json"},
		{"servers", "--url", "http://127.0.0.1:8080", "--url", "http://127.0.0.1:8081"},
		{"servers", "--config", "a.json", "--json", `{}`},
		{"servers", "--config", "a.json", "--arg", "x=1"},
		{"servers", "--config", "a.json", "--listen", "127.0.0.1:8080"},
		{"tools", "s", "--config", "a.json", "--json", `{}`},
		{"serve", "--config", "a.json", "--json", `{}`},
		{"serve", "--config", "a.json", "--arg", "x=1"},
		{"serve", "--config", "a.json", "--listen", "127.0.0.1:8080", "--listen", "127.0.0.1:8081"},
		{"serve", "--config", "a.json", "--listen", "127.0.0.1:notaport"},
		{"stdio", "--config", "a.json", "--listen", "127.0.0.1:8080"},
		{"stdio", "--config", "a.json", "--json", `{}`},
	} {
		var out, err bytes.Buffer
		wantCode := `"code":"USAGE"`
		if len(args) == 5 && args[4] == "127.0.0.1:notaport" {
			wantCode = `"code":"LOOPBACK_REQUIRED"`
		}
		if c := run(context.Background(), args, strings.NewReader(""), &out, &err); c != 2 || !json.Valid(out.Bytes()) || !strings.Contains(out.String(), wantCode) {
			t.Fatalf("args %v returned %d (%s), want exit 2 with %s", args, c, out.String(), wantCode)
		}
	}
}

type brokenOut struct{}

func (brokenOut) Write([]byte) (int, error) { return 0, errors.New("broken stdout") }

func TestCLIBrokenStdoutAndSignals(t *testing.T) {
	if os.Getenv("LMMCP_CLI_SUBPROC") == "1" {
		os.Args = []string{"lmmcp", "stdio", "--config", os.Getenv("LMMCP_CLI_CFG")}
		main()
		return
	}
	var errBuf bytes.Buffer
	if c := run(context.Background(), []string{"--version"}, strings.NewReader(""), brokenOut{}, &errBuf); c != 3 || errBuf.Len() != 0 {
		t.Fatalf("broken stdout returned exit %d, stderr=%q; want exit 3 and empty stderr", c, errBuf.String())
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(t.TempDir(), "cfg.json")
	if err := os.WriteFile(cfgPath, []byte(`{"mcpServers":{"s":{"command":"echo"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
		pr, pw, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(exe, "-test.run=TestCLIBrokenStdoutAndSignals")
		cmd.Env = append(os.Environ(), "LMMCP_CLI_SUBPROC=1", "LMMCP_CLI_CFG="+cfgPath)
		cmd.Stdin = pr
		stdoutPipe, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		_ = pr.Close()
		_, _ = pw.WriteString("{\"id\":\"1\",\"method\":\"GET\",\"path\":\"/v1/servers\"}\n")
		buf := make([]byte, 512)
		n, err := stdoutPipe.Read(buf)
		if err != nil || !strings.Contains(string(buf[:n]), `"ok":true`) {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			_ = pw.Close()
			t.Fatalf("failed to read initial stdio reply: %v %q", err, string(buf[:n]))
		}
		_ = cmd.Process.Signal(sig)
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case waitErr := <-done:
			_ = pw.Close()
			var exitErr *exec.ExitError
			if !errors.As(waitErr, &exitErr) || exitErr.ExitCode() != 4 {
				t.Fatalf("signal %v: got waitErr=%v, want exit code 4", sig, waitErr)
			}
		case <-time.After(1500 * time.Millisecond):
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			_ = pw.Close()
			t.Fatalf("stdio subprocess hung on signal %v with open stdin pipe", sig)
		}
	}
}
