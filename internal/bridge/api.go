package bridge

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
)

func idValid(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}
func segments(path string) ([]string, error) {
	if !strings.HasPrefix(path, "/") || strings.ContainsAny(path, "?#") {
		return nil, bad("INVALID_PATH")
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for i, p := range parts {
		v, e := url.PathUnescape(p)
		if e != nil || strings.ContainsRune(v, 0) {
			return nil, bad("INVALID_PATH")
		}
		parts[i] = v
	}
	return parts, nil
}
func (g *Gateway) Dispatch(ctx context.Context, r Request) Response {
	if ctx.Err() != nil {
		return failure(499, "CANCELLED")
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(g.ctx, cancel)
	defer stop()
	defer cancel()
	p, e := segments(r.Path)
	if e != nil {
		return failure(400, "INVALID_PATH")
	}
	if r.Method == "DELETE" && len(p) == 3 && p[0] == "v1" && p[1] == "requests" {
		g.mu.Lock()
		target := g.requests[p[2]]
		g.mu.Unlock()
		if target == nil {
			return failure(404, "REQUEST_NOT_FOUND")
		}
		target()
		return success(map[string]bool{"cancelled": true})
	}
	if r.ID == "" {
		r.ID = "auto-" + g.requestID()
	}
	if !idValid(r.ID) {
		return failure(400, "INVALID_ID")
	}
	g.mu.Lock()
	if _, exists := g.requests[r.ID]; exists {
		g.mu.Unlock()
		return failure(409, "DUPLICATE_ID")
	}
	g.requests[r.ID] = cancel
	g.mu.Unlock()
	defer func() { g.mu.Lock(); delete(g.requests, r.ID); g.mu.Unlock() }()
	if r.Method == "GET" && r.Path == "/health" {
		return success(map[string]string{"status": "ok"})
	}
	if r.Method == "GET" && r.Path == "/v1/openapi.json" {
		return Response{200, map[string]string{"Content-Type": "application/json"}, OpenAPI()}
	}
	if r.Method == "GET" && r.Path == "/v1/servers" {
		return g.Safe(success(g.servers()))
	}
	if len(p) < 4 || p[0] != "v1" || p[1] != "servers" {
		return failure(404, "NOT_FOUND")
	}
	if p[3] == "resources" {
		if r.Method != "GET" {
			return failure(405, "METHOD_NOT_ALLOWED")
		}
		if len(p) == 4 {
			return g.catalog(ctx, p[2], "resources")
		}
		if len(p) == 5 && p[4] == "templates" {
			return g.catalog(ctx, p[2], "templates")
		}
		if len(p) == 5 && p[4] == "read" {
			if len(r.Query) != 1 {
				return failure(400, "INVALID_QUERY")
			}
			return g.read(ctx, p[2], r.Query["uri"])
		}
		return failure(404, "NOT_FOUND")
	}
	if p[3] == "prompts" {
		if len(p) == 4 && r.Method == "GET" {
			return g.catalog(ctx, p[2], "prompts")
		}
		if len(p) == 5 && r.Method == "POST" {
			var args map[string]string
			if Decode(r.Body, &args) != nil || args == nil {
				return failure(400, "INVALID_ARGUMENTS")
			}
			return g.prompt(ctx, p[2], p[4], args)
		}
		return failure(405, "METHOD_NOT_ALLOWED")
	}
	if p[3] != "tools" {
		return failure(404, "NOT_FOUND")
	}
	if len(p) == 4 && r.Method == "GET" {
		tools, _, err := g.tools(ctx, p[2])
		if err != nil {
			return errorResponse(ctx, err)
		}
		return g.Safe(success(tools))
	}
	if len(p) == 6 && p[5] == "call" && r.Method == "POST" {
		var args map[string]any
		if Decode(r.Body, &args) != nil || args == nil {
			return failure(400, "INVALID_ARGUMENTS")
		}
		return g.invoke(ctx, p[2], p[4], args, r.ID)
	}
	return failure(405, "METHOD_NOT_ALLOWED")
}
func (g *Gateway) Handler(token, host string) http.Handler {
	if token != "" {
		g.addSecrets(token)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		write := func(out Response) {
			out = g.Safe(out)
			for k, v := range out.Headers {
				w.Header().Set(k, v)
			}
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.WriteHeader(out.Status)
			_ = json.NewEncoder(w).Encode(out.Body)
		}
		if host != "" && r.Host != host || r.Header.Get("Origin") != "" {
			write(failure(403, "ORIGIN_REFUSED"))
			return
		}
		if token != "" && (len(r.Header.Values("Authorization")) != 1 || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1) {
			write(failure(401, "UNAUTHORIZED"))
			return
		}
		if r.Method == "POST" {
			kind, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil || kind != "application/json" {
				write(failure(400, "CONTENT_TYPE"))
				return
			}
		}
		query := map[string]string{}
		for k, values := range r.URL.Query() {
			if len(values) != 1 {
				write(failure(400, "INVALID_QUERY"))
				return
			}
			query[k] = values[0]
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			write(failure(400, "INVALID_BODY"))
			return
		}
		write(g.Dispatch(r.Context(), Request{ID: r.Header.Get("X-Request-ID"), Method: r.Method, Path: r.URL.EscapedPath(), Query: query, Body: body}))
	})
}
func Exit(r Response) int {
	if r.Status < 400 {
		if m, ok := r.Body.(map[string]any); ok && m["ok"] == false {
			return 3
		}
		return 0
	}
	if r.Status == 499 {
		return 4
	}
	if r.Status >= 500 || r.Status == 403 || r.Status == 401 {
		return 3
	}
	return 2
}
