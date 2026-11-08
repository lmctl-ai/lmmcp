package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/lmctl-ai/lmmcp/internal/bridge"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

type options struct {
	command, config, url, listen, json, tokenFile, tokenEnv string
	args, positionals                                       []string
	readonly                                                bool
}

func parse(args []string) (options, error) {
	o := options{listen: "127.0.0.1:8080"}
	if len(args) == 0 {
		return o, fmt.Errorf("usage")
	}
	o.command = args[0]
	if o.command == "openapi" && len(args) > 1 {
		return o, fmt.Errorf("usage")
	}
	seen := map[string]bool{}
	for i := 1; i < len(args); i++ {
		word := args[i]
		if word == "--" {
			o.positionals = append(o.positionals, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(word, "--") {
			o.positionals = append(o.positionals, word)
			continue
		}
		key, value, equals := strings.Cut(word, "=")
		if key == "--read-only" {
			if equals || seen[key] {
				return o, fmt.Errorf("usage")
			}
			seen[key] = true
			o.readonly = true
			continue
		}
		if !equals {
			if i+1 == len(args) {
				return o, fmt.Errorf("usage")
			}
			i++
			value = args[i]
		}
		switch key {
		case "--config":
			if seen[key] {
				return o, fmt.Errorf("usage")
			}
			seen[key] = true
			o.config = value
		case "--url":
			if seen[key] {
				return o, fmt.Errorf("usage")
			}
			seen[key] = true
			o.url = value
		case "--listen":
			if seen[key] {
				return o, fmt.Errorf("usage")
			}
			seen[key] = true
			o.listen = value
		case "--json":
			if o.json != "" {
				return o, fmt.Errorf("usage")
			}
			o.json = value
		case "--arg":
			o.args = append(o.args, value)
		case "--token-file":
			if seen[key] {
				return o, fmt.Errorf("usage")
			}
			seen[key] = true
			o.tokenFile = value
		case "--token-env":
			if seen[key] {
				return o, fmt.Errorf("usage")
			}
			seen[key] = true
			o.tokenEnv = value
		default:
			return o, fmt.Errorf("usage")
		}
	}
	if o.config != "" && o.url != "" || o.json != "" && len(o.args) > 0 || o.tokenFile != "" && o.tokenEnv != "" {
		return o, fmt.Errorf("usage")
	}
	if seen["--listen"] && o.command != "serve" {
		return o, fmt.Errorf("usage")
	}
	if (o.json != "" || len(o.args) > 0) && o.command != "call" && o.command != "prompt" {
		return o, fmt.Errorf("usage")
	}
	return o, nil
}
func writeJSON(w io.Writer, v any) bool {
	return json.NewEncoder(w).Encode(v) == nil
}

// configCode keeps the specific configuration error (HEADER_REFERENCE_REQUIRED, INVALID_REMOTE_URL, ...) instead of a
// generic one; it never carries file contents.
func configCode(e error) string {
	var be *bridge.Error
	if errors.As(e, &be) && be.Code != "" {
		return be.Code
	}
	return "INVALID_CONFIG"
}

// abortOnSecondSignal releases session teardown requests that wait on an unresponsive server when the operator
// repeats the shutdown signal. The first signal starts an orderly shutdown; no timer is involved.
func abortOnSecondSignal(ctx context.Context, g *bridge.Gateway) func() {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
		case <-done:
			return
		}
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
		defer signal.Stop(sig)
		select {
		case <-sig:
			g.Abort()
		case <-done:
		}
	}()
	return func() { close(done) }
}
func emit(w io.Writer, status int, code string) int {
	if !writeJSON(w, map[string]any{"ok": false, "error": map[string]string{"code": code, "message": strings.ToLower(strings.ReplaceAll(code, "_", " "))}}) {
		return 3
	}
	return status
}
func auth(o options) (string, error) {
	var text string
	if o.tokenFile != "" {
		b, e := os.ReadFile(o.tokenFile)
		if e != nil {
			return "", e
		}
		text = string(b)
		text = strings.TrimSuffix(strings.TrimSuffix(text, "\n"), "\r")
	} else if o.tokenEnv != "" {
		if !envName(o.tokenEnv) {
			return "", fmt.Errorf("auth")
		}
		var ok bool
		text, ok = os.LookupEnv(o.tokenEnv)
		if !ok {
			return "", fmt.Errorf("auth")
		}
	} else {
		return "", nil
	}
	if text == "" {
		return "", fmt.Errorf("auth")
	}
	for _, c := range text {
		if c < '!' || c > '~' {
			return "", fmt.Errorf("auth")
		}
	}
	return text, nil
}
func envName(s string) bool {
	if s == "" {
		return false
	}
	for i, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' || i > 0 && c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}
func loopback(address string) bool {
	host, portText, e := net.SplitHostPort(address)
	if e != nil {
		return false
	}
	port, pErr := strconv.Atoi(portText)
	if pErr != nil || port < 0 || port > 65535 {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
func request(o options) (bridge.Request, error) {
	r := bridge.Request{Method: "GET"}
	esc := url.PathEscape
	switch o.command {
	case "servers":
		if len(o.positionals) != 0 {
			return r, fmt.Errorf("usage")
		}
		r.Path = "/v1/servers"
	case "tools", "resources", "templates", "prompts":
		if len(o.positionals) != 1 {
			return r, fmt.Errorf("usage")
		}
		kind := o.command
		if kind == "templates" {
			kind = "resources/templates"
		}
		r.Path = "/v1/servers/" + esc(o.positionals[0]) + "/" + kind
	case "read":
		if len(o.positionals) != 2 {
			return r, fmt.Errorf("usage")
		}
		r.Path = "/v1/servers/" + esc(o.positionals[0]) + "/resources/read"
		r.Query = map[string]string{"uri": o.positionals[1]}
	case "call", "prompt":
		if len(o.positionals) != 2 {
			return r, fmt.Errorf("usage")
		}
		r.Method = "POST"
		if o.command == "call" {
			r.Path = "/v1/servers/" + esc(o.positionals[0]) + "/tools/" + esc(o.positionals[1]) + "/call"
		} else {
			r.Path = "/v1/servers/" + esc(o.positionals[0]) + "/prompts/" + esc(o.positionals[1])
		}
		if o.json != "" {
			r.Body = json.RawMessage(o.json)
		} else {
			b, e := bridge.ArgValues(o.args)
			if e != nil {
				return r, e
			}
			r.Body = b
		}
		var data map[string]any
		if bridge.Decode(r.Body, &data) != nil || data == nil {
			return r, fmt.Errorf("usage")
		}
	default:
		return r, fmt.Errorf("usage")
	}
	return r, nil
}
func remote(ctx context.Context, o options, r bridge.Request) (bridge.Response, error) {
	u, e := url.Parse(o.url)
	if e != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" && u.Path != "/" {
		return bridge.Response{}, fmt.Errorf("url")
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "https" && (u.Scheme != "http" || ip == nil || !ip.IsLoopback()) || ip != nil && bridge.ForbiddenAddress(ip) {
		return bridge.Response{}, fmt.Errorf("url")
	}
	token, e := auth(o)
	if e != nil {
		return bridge.Response{}, fmt.Errorf("auth")
	}
	endpoint := strings.TrimSuffix(o.url, "/") + r.Path
	if len(r.Query) > 0 {
		q := url.Values{}
		for k, v := range r.Query {
			q.Set(k, v)
		}
		endpoint += "?" + q.Encode()
	}
	req, e := http.NewRequestWithContext(ctx, r.Method, endpoint, strings.NewReader(string(r.Body)))
	if e != nil {
		return bridge.Response{}, e
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Transport: &http.Transport{Proxy: nil, DialContext: bridge.SafeDialer().DialContext}, CheckRedirect: func(*http.Request, []*http.Request) error { return fmt.Errorf("redirect refused") }}
	res, e := client.Do(req)
	if e != nil {
		return bridge.Response{}, e
	}
	defer res.Body.Close()
	var data any
	d := json.NewDecoder(res.Body)
	d.UseNumber()
	if d.Decode(&data) != nil {
		return bridge.Response{}, fmt.Errorf("response")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return bridge.Response{}, fmt.Errorf("response")
	}
	m, valid := data.(map[string]any)
	if !valid {
		return bridge.Response{}, fmt.Errorf("response")
	}
	ok, valid := m["ok"].(bool)
	if !valid || ok != (res.StatusCode < 400) && !(res.StatusCode == 200 && !ok && toolFailure(m)) {
		return bridge.Response{}, fmt.Errorf("response")
	}
	return bridge.Protect(bridge.Response{Status: res.StatusCode, Body: data}, []string{token}), nil
}
func run(ctx context.Context, args []string, in io.Reader, out, errout io.Writer) int {
	if hostMode(args) {
		return runHost(ctx, args, out)
	}
	if len(args) == 1 && args[0] == "--version" {
		if !writeJSON(out, map[string]string{"name": "lmmcp", "version": "0.1.0"}) {
			return 3
		}
		return 0
	}
	o, e := parse(args)
	if e != nil {
		return emit(out, 2, "USAGE")
	}
	if o.command == "openapi" {
		if !writeJSON(out, bridge.OpenAPI()) {
			return 3
		}
		return 0
	}
	if o.command == "serve" && !loopback(o.listen) {
		return emit(out, 2, "LOOPBACK_REQUIRED")
	}
	var r bridge.Request
	if o.command != "serve" && o.command != "stdio" {
		r, e = request(o)
		if e != nil {
			return emit(out, 2, "USAGE")
		}
	} else if len(o.positionals) > 0 || o.url != "" {
		return emit(out, 2, "USAGE")
	}
	if o.url != "" {
		if o.readonly {
			return emit(out, 2, "SET_READ_ONLY_ON_SERVER")
		}
		res, e := remote(ctx, o, r)
		if e != nil {
			if e.Error() == "url" || e.Error() == "auth" {
				return emit(out, 2, "CLIENT_CONFIG")
			}
			if ctx.Err() != nil {
				return emit(out, 4, "CANCELLED")
			}
			return emit(out, 3, "SERVER_ERROR")
		}
		if !writeJSON(out, res.Body) {
			return 3
		}
		return bridge.Exit(res)
	}
	if o.config == "" {
		return emit(out, 2, "CONFIG_REQUIRED")
	}
	raw, e := os.ReadFile(o.config)
	if e != nil {
		return emit(out, 2, "CONFIG_UNREADABLE")
	}
	cfg, e := bridge.Load(raw)
	if e != nil {
		return emit(out, 2, configCode(e))
	}
	var logMu sync.Mutex
	g, e := bridge.New(cfg, bridge.Options{ReadOnly: o.readonly, Event: func(event bridge.Event) { logMu.Lock(); defer logMu.Unlock(); json.NewEncoder(errout).Encode(event) }})
	if e != nil {
		return emit(out, 2, "INVALID_CONFIG")
	}
	defer g.Close()
	defer abortOnSecondSignal(ctx, g)()
	switch o.command {
	case "stdio":
		if o.tokenFile != "" || o.tokenEnv != "" {
			return emit(out, 2, "USAGE")
		}
		return g.Lines(ctx, in, out)
	case "serve":
		token, e := auth(o)
		if e != nil {
			return emit(out, 2, "AUTH_CONFIG")
		}
		listener, e := net.Listen("tcp", o.listen)
		if e != nil {
			return emit(out, 3, "LISTEN_FAILED")
		}
		srv := &http.Server{Handler: g.Handler(token, listener.Addr().String())}
		stop := context.AfterFunc(ctx, func() { g.Close(); _ = srv.Close() })
		defer stop()
		if !writeJSON(out, map[string]any{"ok": true, "url": "http://" + listener.Addr().String()}) {
			_ = srv.Close()
			return 3
		}
		e = srv.Serve(listener)
		if ctx.Err() != nil {
			return 4
		}
		if e != nil && e != http.ErrServerClosed {
			return 3
		}
		return 0
	default:
		if o.tokenFile != "" || o.tokenEnv != "" {
			return emit(out, 2, "USAGE")
		}
		res := g.Safe(g.Dispatch(ctx, r))
		if !writeJSON(out, res.Body) {
			return 3
		}
		return bridge.Exit(res)
	}
}
func main() {
	signal.Ignore(syscall.SIGPIPE)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	code := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func toolFailure(m map[string]any) bool {
	e, ok := m["error"].(map[string]any)
	if !ok || e["code"] != "TOOL_ERROR" {
		return false
	}
	d, ok := m["data"].(map[string]any)
	return ok && d["isError"] == true
}
