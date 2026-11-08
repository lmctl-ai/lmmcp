package bridge

import (
	"context"
	"errors"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
)

func remoteKind(c ServerConfig) string {
	if c.Transport != "" {
		return c.Transport
	}
	if c.Type != "" {
		return c.Type
	}
	return "http"
}
func networkURL(raw string) (*url.URL, error) {
	u, e := url.Parse(raw)
	if e != nil || u.Host == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" {
		return nil, bad("INVALID_REMOTE_URL")
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "https" && (u.Scheme != "http" || ip == nil || !ip.IsLoopback()) {
		return nil, bad("INVALID_REMOTE_URL")
	}
	return u, nil
}
func headerName(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", c)) {
			return false
		}
	}
	return true
}
func validateRemote(c ServerConfig) error {
	if c.URL == "" {
		if c.Transport != "" && c.Transport != "stdio" || c.Type != "" && c.Type != "stdio" || len(c.Headers) > 0 || c.ProtocolVersion != "" {
			return bad("INVALID_TRANSPORT")
		}
		return nil
	}
	if _, e := networkURL(c.URL); e != nil {
		return e
	}
	if c.Type != "" && c.Transport != "" && c.Type != c.Transport {
		return bad("INVALID_TRANSPORT")
	}
	switch remoteKind(c) {
	case "http", "streamable-http", "sse", "auto":
	default:
		return bad("INVALID_TRANSPORT")
	}
	switch c.ProtocolVersion {
	case "", "2026-07-28", "2025-11-25", "2024-11-05":
	default:
		return bad("INVALID_PROTOCOL_VERSION")
	}
	seen := map[string]bool{}
	for k, v := range c.Headers {
		key := strings.ToLower(k)
		if !headerName(k) || seen[key] {
			return bad("INVALID_HEADER")
		}
		seen[key] = true
		switch key {
		case "content-type", "accept", "content-length", "connection", "transfer-encoding", "mcp-session-id", "mcp-protocol-version", "mcp-method", "mcp-name":
			return bad("RESERVED_HEADER")
		}
		if !strings.Contains(v, "${") {
			return bad("HEADER_REFERENCE_REQUIRED")
		}
		if _, _, e := expand(v, func(string) (string, bool) { return "fixture", true }); e != nil {
			return e
		}
	}
	return nil
}

type remoteState struct {
	ctx     context.Context
	cancel  context.CancelFunc
	status  atomic.Int64
	expired atomic.Bool
	headers http.Header
	origin  *url.URL
	base    *http.Transport
}

func (r *remoteState) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != r.origin.Scheme || !strings.EqualFold(req.URL.Host, r.origin.Host) {
		return nil, bad("REMOTE_ORIGIN_CHANGED")
	}
	ctx := req.Context()
	if req.Method == "DELETE" {
		ctx = context.WithoutCancel(ctx)
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(r.ctx, cancel)
	if r.ctx.Err() != nil {
		cancel()
	}
	clone := req.Clone(ctx)
	clone.Header = req.Header.Clone()
	for k, v := range r.headers {
		if strings.EqualFold(k, "Host") {
			clone.Host = v[0]
		} else {
			clone.Header[k] = v
		}
	}
	res, err := r.base.RoundTrip(clone)
	if err != nil {
		stop()
		cancel()
		return nil, err
	}
	if res.StatusCode >= 400 {
		r.status.Store(int64(res.StatusCode))
		if res.StatusCode == 404 && req.Header.Get("Mcp-Session-Id") != "" && req.Method == "POST" {
			r.expired.Store(true)
		}
	}
	res.Body = &remoteBody{ReadCloser: res.Body, stop: func() { stop(); cancel() }}
	return res, nil
}

type remoteBody struct {
	io.ReadCloser
	stop func()
}

func (b *remoteBody) Close() error { e := b.ReadCloser.Close(); b.stop(); return e }
func (g *Gateway) connectRemote(ctx context.Context, client *mcp.Client, c ServerConfig) (*mcp.ClientSession, *remoteState, error) {
	u, err := networkURL(c.URL)
	if err != nil {
		return nil, nil, err
	}
	life, cancel := context.WithCancel(g.ctx)
	r := &remoteState{ctx: life, cancel: cancel, origin: u, headers: http.Header{}, base: &http.Transport{Proxy: nil}}
	for k, v := range c.Headers {
		value, secrets, e := expand(v, g.opts.Lookup)
		if e != nil {
			cancel()
			return nil, nil, e
		}
		if strings.ContainsAny(value, "\r\n\x00") {
			cancel()
			return nil, nil, bad("INVALID_HEADER")
		}
		g.addSecrets(secrets...)
		r.headers.Set(k, value)
	}
	hc := &http.Client{Transport: r, CheckRedirect: func(*http.Request, []*http.Request) error { return bad("REDIRECT_REFUSED") }}
	kind := remoteKind(c)
	version := c.ProtocolVersion
	if version == "" {
		if kind == "sse" {
			version = "2024-11-05"
		} else if kind == "auto" {
			version = "2025-11-25"
		}
	}
	var transport mcp.Transport
	makeStream := func() mcp.Transport {
		return &mcp.StreamableClientTransport{Endpoint: c.URL, HTTPClient: hc, MaxRetries: -1, MaxEventSize: -1}
	}
	if kind == "sse" {
		transport = &mcp.SSEClientTransport{Endpoint: c.URL, HTTPClient: hc, MaxEventSize: -1}
	} else {
		transport = makeStream()
	}
	session, err := client.Connect(ctx, transport, &mcp.ClientSessionOptions{ProtocolVersion: version})
	if err != nil && kind == "auto" && r.status.Load() >= 400 && r.status.Load() < 500 && r.status.Load() != 401 && r.status.Load() != 403 && ctx.Err() == nil {
		endpoint := *u
		endpoint.Path = "/sse"
		endpoint.RawPath = ""
		r.status.Store(0)
		session, err = client.Connect(ctx, &mcp.SSEClientTransport{Endpoint: endpoint.String(), HTTPClient: hc, MaxEventSize: -1}, &mcp.ClientSessionOptions{ProtocolVersion: "2024-11-05"})
	}
	if err != nil {
		cancel()
		r.base.CloseIdleConnections()
		return nil, nil, remoteError(r, err)
	}
	return session, r, nil
}
func remoteError(r *remoteState, err error) error {
	switch r.status.Load() {
	case 401:
		return bad("UPSTREAM_UNAUTHORIZED")
	case 403:
		return bad("UPSTREAM_FORBIDDEN")
	}
	if r.expired.Load() {
		return bad("SESSION_EXPIRED")
	}
	return bad("UPSTREAM_FAILED")
}
func operation[T any](g *Gateway, ctx context.Context, name string, fn func(*mcp.ClientSession) (T, error)) (T, error) {
	var zero T
	for attempt := 0; attempt < 2; attempt++ {
		e, err := g.get(ctx, name)
		if err != nil {
			return zero, err
		}
		<-e.gate
		s, r := e.session, e.remote
		e.gate <- struct{}{}
		if s == nil {
			return zero, bad("UPSTREAM_FAILED")
		}
		value, err := fn(s)
		if err == nil {
			return value, nil
		}
		if ctx.Err() != nil {
			return zero, bad("CANCELLED")
		}
		expired := r != nil && (r.expired.Load() || errors.Is(err, mcp.ErrSessionMissing))
		if r != nil {
			err = remoteError(r, err)
		} else {
			err = bad("UPSTREAM_FAILED")
		}
		g.invalidateSession(e, s)
		if !expired {
			return zero, err
		}
		if attempt == 1 {
			return zero, bad("SESSION_EXPIRED")
		}
	}
	return zero, bad("SESSION_EXPIRED")
}
func (g *Gateway) invalidateSession(e *entry, s *mcp.ClientSession) {
	<-e.gate
	if e.session == s {
		if e.remote != nil {
			e.remote.cancel()
			e.remote.base.CloseIdleConnections()
			e.remote = nil
		}
		_ = e.session.Close()
		e.session = nil
		if e.process != nil {
			e.process.Close()
		}
	}
	e.gate <- struct{}{}
}
