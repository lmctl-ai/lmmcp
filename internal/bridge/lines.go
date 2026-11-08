package bridge

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
)

type LineRequest struct {
	ID     json.RawMessage   `json:"id"`
	Method string            `json:"method"`
	Path   string            `json:"path"`
	Query  map[string]string `json:"query,omitempty"`
	Body   json.RawMessage   `json:"body,omitempty"`
}

func lineID(raw json.RawMessage) (string, bool) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, idValid(s)
	}
	s = string(raw)
	if s == "" {
		return "", false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return "", false
		}
	}
	return s, true
}

func partialID(line []byte) json.RawMessage {
	var probe struct {
		ID json.RawMessage `json:"id"`
	}
	if json.Unmarshal(line, &probe) == nil {
		if _, ok := lineID(probe.ID); ok {
			return probe.ID
		}
	}
	return nil
}

func (g *Gateway) Lines(ctx context.Context, in io.Reader, out io.Writer) int {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var mu sync.Mutex
	var wg sync.WaitGroup
	status := 0
	send := func(id json.RawMessage, r Response) {
		mu.Lock()
		defer mu.Unlock()
		if id == nil {
			id = json.RawMessage("null")
		}
		r = g.Safe(r)
		if json.NewEncoder(out).Encode(struct {
			ID json.RawMessage `json:"id"`
			Response
		}{id, r}) != nil {
			cancel()
			status = 3
		}
	}
	polled, cleanup := openPolledReader(ctx, in)
	defer cleanup()
	reader := bufio.NewReader(polled)
	stop := context.AfterFunc(ctx, func() {
		if _, isFile := in.(*os.File); !isFile {
			if closer, ok := in.(io.Closer); ok {
				_ = closer.Close()
			}
		}
	})
	defer stop()
	for {
		line, err := reader.ReadBytes('\n')
		if len(strings.TrimSpace(string(line))) > 0 {
			if len(line) == 0 || line[len(line)-1] != '\n' {
				if ctx.Err() == nil {
					send(partialID(line), failure(400, "INVALID_REQUEST"))
				}
			} else {
				var r LineRequest
				if decErr := Decode(line, &r); decErr != nil {
					var codeErr *Error
					if errors.As(decErr, &codeErr) && codeErr.Code == "INVALID_UTF8" {
						send(partialID(line), Response{400, map[string]string{"Content-Type": "application/json"}, map[string]any{"ok": false, "error": codeErr}})
					} else {
						send(partialID(line), failure(400, "INVALID_REQUEST"))
					}
				} else if id, ok := lineID(r.ID); !ok {
					send(nil, failure(400, "INVALID_ID"))
				} else {
					wg.Add(1)
					go func(r LineRequest, id string) {
						defer wg.Done()
						send(r.ID, g.Dispatch(ctx, Request{ID: id, Method: r.Method, Path: r.Path, Query: r.Query, Body: r.Body}))
					}(r, id)
				}
			}
		}
		if err != nil {
			if err != io.EOF && ctx.Err() == nil {
				send(nil, failure(400, "INPUT_ERROR"))
			}
			break
		}
	}
	wg.Wait()
	if status != 0 {
		return status
	}
	if ctx.Err() != nil {
		return 4
	}
	return status
}
func ArgValues(items []string) (json.RawMessage, error) {
	m := map[string]string{}
	for _, item := range items {
		k, v, ok := strings.Cut(item, "=")
		if !ok || k == "" {
			return nil, bad("INVALID_ARGUMENTS")
		}
		if _, exists := m[k]; exists {
			return nil, bad("INVALID_ARGUMENTS")
		}
		m[k] = v
	}
	b, e := json.Marshal(m)
	return b, e
}
