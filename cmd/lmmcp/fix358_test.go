package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func cli(args ...string) (int, string) {
	var out, errout bytes.Buffer
	c := run(context.Background(), args, strings.NewReader(""), &out, &errout)
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

// The REST client never dials link-local or metadata destinations.
func TestClientRefusesMetadataDestinations(t *testing.T) {
	for _, u := range []string{"https://169.254.169.254", "https://[fe80::1]", "https://0.0.0.0", "https://[fd00:ec2::254]"} {
		if c, out := cli("servers", "--url", u); c != 2 || !strings.Contains(out, "CLIENT_CONFIG") {
			t.Errorf("%s: exit %d %s", u, c, out)
		}
	}
}
