package bridge

import (
	"context"
	"errors"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
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
func allowPrivate(c ServerConfig) bool {
	return c.AllowPrivateAddresses == nil || *c.AllowPrivateAddresses
}

// networkURL is the whole URL policy: https to any host (literal link-local, metadata and unspecified addresses
// refused, and refused again at dial time after DNS resolution), plain http only to a literal loopback address, no
// userinfo, query or fragment, a real port, no NUL in the path. With allowPrivate false, literal private addresses are
// refused here and every private destination at dial time.
func networkURL(raw string, allowPrivate bool) (*url.URL, error) {
	u, e := url.Parse(raw)
	if e != nil || u.Host == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" || strings.Contains(raw, "?") || strings.Contains(raw, "#") || strings.ContainsRune(u.Path, 0) {
		return nil, bad("INVALID_REMOTE_URL")
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return nil, bad("INVALID_REMOTE_URL")
		}
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "https" && (u.Scheme != "http" || ip == nil || !ip.IsLoopback()) {
		return nil, bad("INVALID_REMOTE_URL")
	}
	if ip != nil {
		if forbiddenAddress(ip) {
			return nil, bad("INVALID_REMOTE_URL")
		}
		if !allowPrivate && privateAddress(ip) && !(u.Scheme == "http" && ip.IsLoopback()) {
			return nil, bad("INVALID_REMOTE_URL")
		}
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
		if c.Transport != "" && c.Transport != "stdio" || c.Type != "" && c.Type != "stdio" || len(c.Headers) > 0 || c.ProtocolVersion != "" || c.AllowPrivateAddresses != nil {
			return bad("INVALID_TRANSPORT")
		}
		return nil
	}
	if _, e := networkURL(c.URL, allowPrivate(c)); e != nil {
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
		if strings.ContainsAny(v, "\r\n\x00") {
			return bad("INVALID_HEADER")
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
	ctx        context.Context // lifetime of the connection; cancelled after the session DELETE
	cancel     context.CancelFunc
	abort      context.Context // ends teardown requests only on an explicit Abort
	status     atomic.Int64    // last >=400 status seen while connecting
	refused    atomic.Bool     // the dial-time address policy refused a destination
	client     *http.Client
	endpoint   string      // Streamable HTTP endpoint ("" for legacy SSE: nothing to DELETE)
	skipDelete atomic.Bool // our own DELETE already ended the session; the SDK's is answered locally
	headers    http.Header
	origin     *url.URL
	base       *http.Transport
}

// callTrace records what happened to the HTTP requests issued under one logical call. Retry decisions come from this
// trace only, never from shared session state.
type callTrace struct {
	sent     atomic.Int32 // POST requests whose write began
	rejected atomic.Int32 // POST requests answered 404 for their session id
	other    atomic.Int32 // POST requests that got any other outcome (response, transport error)
	status   atomic.Int64 // last >=400 status of this call
}
type traceKey struct{}

// mayResend reports whether the call may be sent again: either nothing was written (the session was already dead),
// or its own single POST was answered 404 for the session id (rejected before execution).
func (t *callTrace) mayResend(sessionDead bool) bool {
	if t.sent.Load() == 0 {
		return sessionDead
	}
	return t.sent.Load() == 1 && t.rejected.Load() == 1 && t.other.Load() == 0
}

func sameOrigin(a, b *url.URL) bool {
	port := func(u *url.URL) string {
		if p := u.Port(); p != "" {
			return p
		}
		if u.Scheme == "https" {
			return "443"
		}
		return "80"
	}
	return a.Scheme == b.Scheme && strings.EqualFold(a.Hostname(), b.Hostname()) && port(a) == port(b)
}

// terminate ends a Streamable HTTP session with our own DELETE BEFORE the SDK connection is closed. The SDK sends its
// DELETE while holding the connection's in-flight lock, which would block a pending cancellation notification the
// server may be waiting for; ours holds no lock. No deadline: Abort releases it.
func (r *remoteState) terminate(session *mcp.ClientSession) {
	defer r.skipDelete.Store(true)
	if r.endpoint == "" || session.ID() == "" {
		return
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodDelete, r.endpoint, nil)
	if err != nil {
		return
	}
	req.Header.Set("Mcp-Session-Id", session.ID())
	if init := session.InitializeResult(); init != nil && init.ProtocolVersion != "" {
		req.Header.Set("Mcp-Protocol-Version", init.ProtocolVersion)
	}
	if res, err := r.client.Do(req); err == nil {
		res.Body.Close()
	}
}

// cancelNotification recognises a notifications/cancelled POST by its first bytes (JSON-RPC puts "method" before
// "params"); the rest of the body is not read.
func cancelNotification(req *http.Request) bool {
	if req.Method != "POST" || req.GetBody == nil {
		return false
	}
	rc, err := req.GetBody()
	if err != nil {
		return false
	}
	defer rc.Close()
	head, _ := io.ReadAll(io.LimitReader(rc, 256))
	return strings.Contains(string(head), `"method":"notifications/cancelled"`)
}

func (r *remoteState) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.User != nil || !sameOrigin(req.URL, r.origin) {
		return nil, bad("REMOTE_ORIGIN_CHANGED")
	}
	if req.Method == "DELETE" && r.skipDelete.Load() {
		return &http.Response{StatusCode: 200, Status: "200 OK", Header: http.Header{}, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
	}
	ctx, life := req.Context(), r.ctx
	note := cancelNotification(req)
	if req.Method == "DELETE" || note {
		// Session termination and the cancellation notices a server may be waiting for outlive the request and the
		// lifetime context; only Abort ends them.
		ctx, life = context.WithoutCancel(ctx), r.abort
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(life, cancel)
	if life.Err() != nil {
		cancel()
	}
	trace, _ := req.Context().Value(traceKey{}).(*callTrace)
	post := trace != nil && req.Method == "POST" && !note
	if post {
		trace.sent.Add(1)
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
		if post {
			trace.other.Add(1)
		}
		return nil, err
	}
	if res.StatusCode >= 400 {
		r.status.Store(int64(res.StatusCode))
		if trace != nil {
			trace.status.Store(int64(res.StatusCode))
		}
	}
	if post {
		if res.StatusCode == 404 && req.Header.Get("Mcp-Session-Id") != "" {
			trace.rejected.Add(1)
		} else {
			trace.other.Add(1)
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
func (g *Gateway) connectRemote(ctx context.Context, client *mcp.Client, c ServerConfig, name string) (*mcp.ClientSession, *remoteState, error) {
	u, err := networkURL(c.URL, allowPrivate(c))
	if err != nil {
		return nil, nil, err
	}
	life, cancel := context.WithCancel(g.ctx)
	r := &remoteState{ctx: life, cancel: cancel, abort: g.teardown, origin: u, headers: http.Header{}}
	r.base = newRemoteTransport(allowPrivate(c), u.Scheme == "http", func() { r.refused.Store(true) })
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
	r.client = hc
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
	legacy := kind == "sse"
	makeStream := func() mcp.Transport {
		return &mcp.StreamableClientTransport{Endpoint: c.URL, HTTPClient: hc, MaxRetries: -1, MaxEventSize: -1, DisableStandaloneSSE: version == "2025-11-25"}
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
		legacy = true
		session, err = client.Connect(ctx, &mcp.SSEClientTransport{Endpoint: endpoint.String(), HTTPClient: hc, MaxEventSize: -1}, &mcp.ClientSessionOptions{ProtocolVersion: "2024-11-05"})
	}
	if err != nil {
		cancel()
		r.base.CloseIdleConnections()
		return nil, nil, remoteError(r, err)
	}
	if !legacy {
		r.endpoint = c.URL
	}
	if version == "2025-11-25" && kind != "sse" && session.ID() != "" {
		go g.watchChanges(r, hc, c.URL, session.ID(), name)
	}
	return session, r, nil
}

// newRemoteTransport ignores proxy environment variables on purpose (a proxy would defeat the address policy) and
// checks every destination address at dial time.
func newRemoteTransport(allowPrivate, loopbackLiteral bool, refused func()) *http.Transport {
	dialer := &net.Dialer{Control: dialControl(allowPrivate, loopbackLiteral, refused)}
	return &http.Transport{Proxy: nil, DialContext: dialer.DialContext}
}
func remoteError(r *remoteState, err error) error {
	if r.refused.Load() {
		return bad("ADDRESS_REFUSED")
	}
	switch r.status.Load() {
	case 401:
		return bad("UPSTREAM_UNAUTHORIZED")
	case 403:
		return bad("UPSTREAM_FORBIDDEN")
	}
	return bad("UPSTREAM_FAILED")
}

// operation runs fn on the server's current session. At most one retry, decided from this call's own trace.
func operation[T any](g *Gateway, ctx context.Context, name, kind string, fn func(context.Context, *mcp.ClientSession) (T, error)) (T, error) {
	var zero T
	for attempt := 0; attempt < 2; attempt++ {
		e, err := g.get(ctx, name)
		if err != nil {
			if attempt > 0 {
				return zero, bad("SESSION_REINITIALIZE_FAILED")
			}
			return zero, err
		}
		<-e.gate
		s, r := e.session, e.remote
		if s != nil {
			e.users[s]++
		}
		e.gate <- struct{}{}
		if s == nil {
			return zero, bad("UPSTREAM_FAILED")
		}
		trace := &callTrace{}
		value, err := fn(context.WithValue(ctx, traceKey{}, trace), s)
		g.release(e, s)
		if err == nil {
			return value, nil
		}
		if ctx.Err() != nil {
			return zero, bad("CANCELLED")
		}
		dead := errors.Is(err, mcp.ErrSessionMissing)
		if dead || errors.Is(err, mcp.ErrConnectionClosed) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) {
			g.markBroken(e, s) // only a session that cannot carry work is replaced, never because of an MCP error
		}
		resend := r != nil && trace.mayResend(dead)
		failure := classify(err, r, trace, kind)
		if !resend {
			return zero, failure
		}
		if attempt == 1 {
			return zero, bad("SESSION_EXPIRED")
		}
	}
	return zero, bad("SESSION_EXPIRED")
}

// release ends one operation's use of s; the last user of a retired connection closes it.
func (g *Gateway) release(e *entry, s *mcp.ClientSession) {
	<-e.gate
	e.users[s]--
	if e.users[s] <= 0 {
		delete(e.users, s)
		if ret := e.retired[s]; ret != nil {
			delete(e.retired, s)
			g.closeConn(s, ret.remote, ret.process)
		}
	}
	e.gate <- struct{}{}
}

// markBroken flags the current session as unusable for new work. Operations already running on it are not touched.
func (g *Gateway) markBroken(e *entry, s *mcp.ClientSession) {
	<-e.gate
	if e.session == s {
		e.broken = s
	}
	e.gate <- struct{}{}
}
