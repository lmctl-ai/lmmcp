package bridge

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Code }
func bad(code string) *Error {
	return &Error{code, strings.ToLower(strings.ReplaceAll(code, "_", " "))}
}

type Request struct {
	ID     string
	Method string
	Path   string
	Query  map[string]string
	Body   json.RawMessage
}
type Response struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
	Body    any               `json:"body"`
}

func failure(status int, code string) Response {
	return Response{status, map[string]string{"Content-Type": "application/json"}, map[string]any{"ok": false, "error": bad(code)}}
}
func success(data any) Response {
	return Response{200, map[string]string{"Content-Type": "application/json"}, map[string]any{"ok": true, "data": data}}
}

type Event struct {
	Kind     string  `json:"event"`
	Server   string  `json:"server,omitempty"`
	ID       string  `json:"id,omitempty"`
	Progress float64 `json:"progress,omitempty"`
	Total    float64 `json:"total,omitempty"`
}
type Options struct {
	ReadOnly bool
	Lookup   func(string) (string, bool)
	Event    func(Event)
}
type entry struct {
	changed atomic.Bool
	active  int
	gate    chan struct{}
	session *mcp.ClientSession
	tools   []*mcp.Tool
	process *child
	remote  *remoteState
}
type child struct {
	command *exec.Cmd
	reader  io.ReadCloser
	writer  io.WriteCloser
	once    sync.Once
	state   *os.ProcessState
}

func (c *child) Read(b []byte) (int, error) { return c.reader.Read(b) }
func (c *child) Close() error {
	c.once.Do(func() {
		_ = c.writer.Close()
		stopProcess(c.command)
		_ = c.reader.Close()
		_ = c.command.Wait()
		c.state = c.command.ProcessState
	})
	return nil
}

type Gateway struct {
	cfg      Config
	opts     Options
	ctx      context.Context
	cancel   context.CancelFunc
	mu       sync.Mutex
	entries  map[string]*entry
	closed   bool
	secrets  []string
	requests map[string]context.CancelFunc
	starts   atomic.Int64
	ids      atomic.Uint64
}

func New(c Config, o Options) (*Gateway, error) {
	if o.Lookup == nil {
		o.Lookup = os.LookupEnv
	}
	if o.Event == nil {
		o.Event = func(Event) {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Gateway{cfg: c, opts: o, ctx: ctx, cancel: cancel, entries: map[string]*entry{}, requests: map[string]context.CancelFunc{}}, nil
}
const minSecretLen = 8

func (g *Gateway) StartCount() int64 { return g.starts.Load() }
func (g *Gateway) addSecrets(values ...string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, v := range values {
		if len(v) >= minSecretLen {
			g.secrets = append(g.secrets, v)
		}
	}
}
func (g *Gateway) Safe(r Response) Response {
	g.mu.Lock()
	secrets := append([]string(nil), g.secrets...)
	g.mu.Unlock()
	if len(secrets) == 0 {
		return r
	}
	return Protect(r, secrets)
}
func Protect(r Response, secrets []string) Response {
	if len(secrets) == 0 {
		return r
	}
	b, e := json.Marshal(r.Body)
	if e != nil {
		return failure(502, "INVALID_UPSTREAM_RESULT")
	}
	var data any
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.UseNumber()
	if dec.Decode(&data) != nil {
		return failure(502, "INVALID_UPSTREAM_RESULT")
	}
	var contains func(any) bool
	contains = func(v any) bool {
		switch x := v.(type) {
		case string:
			for _, s := range secrets {
				if len(s) >= minSecretLen && strings.Contains(x, s) {
					return true
				}
			}
		case json.Number:
			num := x.String()
			for _, s := range secrets {
				if len(s) >= minSecretLen && strings.Contains(num, s) {
					return true
				}
			}
		case []any:
			for _, a := range x {
				if contains(a) {
					return true
				}
			}
		case map[string]any:
			for k, a := range x {
				if contains(k) || contains(a) {
					return true
				}
			}
		}
		return false
	}
	if contains(data) {
		return failure(502, "SECRET_OUTPUT")
	}
	return r
}
func (g *Gateway) get(ctx context.Context, name string) (*entry, error) {
	c, ok := g.cfg.Servers[name]
	if !ok {
		return nil, bad("SERVER_NOT_FOUND")
	}
	if c.Disabled {
		return nil, bad("SERVER_DISABLED")
	}

	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return nil, bad("CANCELLED")
	}
	e := g.entries[name]
	if e == nil {
		e = &entry{gate: make(chan struct{}, 1)}
		e.gate <- struct{}{}
		g.entries[name] = e
	}
	g.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil, bad("CANCELLED")
	case <-e.gate:
	}
	defer func() { e.gate <- struct{}{} }()
	if e.session != nil && e.active == 0 && e.changed.Swap(false) {
		if e.remote != nil {
			e.remote.cancel()
			e.remote.base.CloseIdleConnections()
			e.remote = nil
		}
		_ = e.session.Close()
		e.session = nil
		e.tools = nil
		if e.process != nil {
			e.process.Close()
		}
	}
	if e.session != nil {
		return e, nil
	}
	client := g.client(name)
	if c.URL != "" {
		session, state, err := g.connectRemote(ctx, client, c, name)
		if err != nil {
			return nil, err
		}
		e.session = session
		e.remote = state
		return e, nil
	}
	env := []string{}
	inherited := c.EnvAllowlist
	if inherited == nil {
		inherited = []string{"PATH"}
	}
	for _, k := range inherited {
		if v, ok := g.opts.Lookup(k); ok {
			env = append(env, k+"="+v)
			if k != "PATH" && v != "" {
				g.addSecrets(v)
			}
		}
	}
	for k, value := range c.Env {
		v, secrets, err := expand(value, g.opts.Lookup)
		if err != nil {
			return nil, err
		}
		if strings.ContainsRune(v, 0) {
			return nil, bad("INVALID_ENV")
		}
		g.addSecrets(secrets...)
		env = append(env, k+"="+v)
	}
	cmd := exec.Command(c.Command, c.Args...)
	cmd.Env = env
	cmd.Dir = c.Cwd
	null, openErr := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if openErr != nil {
		return nil, bad("START_FAILED")
	}
	defer null.Close()
	cmd.Stderr = null
	prepareProcess(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, bad("START_FAILED")
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, bad("START_FAILED")
	}
	if cmd.Start() != nil {
		stdin.Close()
		stdout.Close()
		return nil, bad("START_FAILED")
	}
	g.starts.Add(1)
	owned := &child{command: cmd, reader: stdout, writer: stdin}
	e.process = owned
	started := Event{Kind: "started", Server: name}
	if g.Safe(success(started)).Status == 200 {
		g.opts.Event(started)
	}

	session, err := client.Connect(ctx, &mcp.IOTransport{Reader: owned, Writer: stdin, MaxLineLength: -1}, nil)
	if err != nil {
		owned.Close()
		return nil, bad("CONNECT_FAILED")
	}
	e.session = session
	return e, nil
}

func (g *Gateway) client(name string) *mcp.Client {
	return mcp.NewClient(&mcp.Implementation{Name: "lmmcp", Version: "0.1.0"}, &mcp.ClientOptions{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), MultiRoundTrip: &mcp.MultiRoundTripOptions{Disabled: true}, ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) {
		g.markChanged(name)
		event := Event{Kind: "list_changed", Server: name}
		if g.Safe(success(event)).Status == 200 {
			g.opts.Event(event)
		}
	}, ProgressNotificationHandler: func(_ context.Context, r *mcp.ProgressNotificationClientRequest) {
		event := Event{Kind: "progress", Server: name, ID: fmt.Sprint(r.Params.ProgressToken), Progress: r.Params.Progress, Total: r.Params.Total}
		if g.Safe(success(event)).Status == 200 {
			g.opts.Event(event)
		}
	}})
}
func (g *Gateway) invalidate(e *entry) {
	<-e.gate
	if e.remote != nil {
		e.remote.cancel()
		e.remote.base.CloseIdleConnections()
		e.remote = nil
	}
	if e.session != nil {
		_ = e.session.Close()
		e.session = nil
	}
	e.tools = nil
	if e.process != nil {
		e.process.Close()
	}
	e.gate <- struct{}{}
}
func (g *Gateway) Close() {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return
	}
	g.closed = true
	g.cancel()
	entries := make([]*entry, 0, len(g.entries))
	for _, e := range g.entries {
		entries = append(entries, e)
	}
	g.mu.Unlock()
	for _, e := range entries {
		g.invalidate(e)
	}
}
func (g *Gateway) tools(ctx context.Context, name string, refresh bool) ([]*mcp.Tool, *entry, error) {
	e, err := g.get(ctx, name)
	if err != nil {
		return nil, nil, err
	}
	<-e.gate
	session := e.session
	cached := e.tools
	e.gate <- struct{}{}
	if session == nil {
		return nil, nil, bad("UPSTREAM_FAILED")
	}
	if !refresh && cached != nil {
		return cached, e, nil
	}
	tools := []*mcp.Tool{}
	cursor := ""
	seen := map[string]bool{}
	for {
		res, err := operation(g, ctx, name, func(s *mcp.ClientSession) (*mcp.ListToolsResult, error) {
			return s.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
		})
		if err != nil {
			return nil, nil, err
		}
		tools = append(tools, res.Tools...)
		if res.NextCursor == "" {
			break
		}
		if seen[res.NextCursor] {
			return nil, nil, bad("INVALID_PAGINATION")
		}
		seen[res.NextCursor] = true
		cursor = res.NextCursor
	}
	<-e.gate
	if e.session != nil {
		e.tools = tools
	}
	e.gate <- struct{}{}
	return tools, e, nil
}
func (g *Gateway) servers() any {
	names := make([]string, 0, len(g.cfg.Servers))
	for n := range g.cfg.Servers {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]map[string]any, 0, len(names))
	for _, n := range names {
		c := g.cfg.Servers[n]
		transport := "stdio"
		if c.URL != "" {
			transport = remoteKind(c)
		}
		out = append(out, map[string]any{"name": n, "transport": transport, "disabled": c.Disabled})
	}
	return out
}
func (g *Gateway) invoke(ctx context.Context, name, tool string, args map[string]any, id string) Response {
	c, ok := g.cfg.Servers[name]
	if !ok {
		return failure(404, "SERVER_NOT_FOUND")
	}
	if has(c.DenyTools, tool) || c.AllowTools != nil && !has(c.AllowTools, tool) {
		return failure(403, "TOOL_DENIED")
	}
	tools, e, err := g.tools(ctx, name, false)
	if err != nil {
		return errorResponse(ctx, err)
	}
	var selected *mcp.Tool
	for _, t := range tools {
		if t.Name == tool {
			selected = t
			break
		}
	}
	if selected == nil {
		return failure(404, "TOOL_NOT_FOUND")
	}
	if g.opts.ReadOnly && (selected.Annotations == nil || !selected.Annotations.ReadOnlyHint || selected.Annotations.DestructiveHint != nil && *selected.Annotations.DestructiveHint) {
		return failure(403, "READ_ONLY")
	}
	<-e.gate
	session := e.session
	e.gate <- struct{}{}
	if session == nil {
		return failure(502, "UPSTREAM_FAILED")
	}
	params := &mcp.CallToolParams{Name: tool, Arguments: args}
	params.SetProgressToken(id)
	res, err := operation(g, ctx, name, func(s *mcp.ClientSession) (*mcp.CallToolResult, error) { return s.CallTool(ctx, params) })
	if err != nil {
		if ctx.Err() != nil {
			return failure(499, "CANCELLED")
		}
		return errorResponse(ctx, err)
	}
	if res.IsError {
		r := failure(200, "TOOL_ERROR")
		r.Body.(map[string]any)["data"] = res
		return g.Safe(r)
	}
	if res.NeedsInput() {
		return failure(502, "INPUT_REQUIRED")
	}
	return g.Safe(success(res))
}
func has(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
func errorResponse(ctx context.Context, e error) Response {
	if ctx.Err() != nil {
		return failure(499, "CANCELLED")
	}
	switch e.Error() {
	case "SERVER_NOT_FOUND":
		return failure(404, "SERVER_NOT_FOUND")
	case "SERVER_DISABLED", "ENV_NOT_SET", "INVALID_ENV", "INVALID_ENV_REFERENCE":
		return failure(400, e.Error())
	case "UNSUPPORTED_TRANSPORT":
		return failure(501, "UNSUPPORTED_TRANSPORT")
	case "CANCELLED":
		return failure(499, "CANCELLED")
	}
	return failure(502, e.Error())
}
func (g *Gateway) requestID() string { return strconv.FormatUint(g.ids.Add(1), 10) }

func (g *Gateway) markChanged(name string) {
	g.mu.Lock()
	if e := g.entries[name]; e != nil {
		e.changed.Store(true)
	}
	g.mu.Unlock()
}
