package main

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/lmctlhq/lmmcp/internal/bridge"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
