package bridge

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
)

// classifyWindow is how much of ONE event's data is kept to recognise a notification. A tools/list_changed
// notification is a few dozen bytes; anything beyond the window belongs to an event we do not use and is read and
// dropped, so memory stays flat however long an event or line is. Nothing delivered to callers is ever truncated.
const classifyWindow = 4096

// watchChanges reads the optional 2025 notification stream incrementally. The stream is never buffered as a whole:
// it is consumed in fixed-size slices, only the first classifyWindow bytes of an event's data are retained, and an
// event is classified when its blank line arrives.
func (g *Gateway) watchChanges(state *remoteState, client *http.Client, endpoint, session, name string) {
	req, e := http.NewRequestWithContext(state.ctx, "GET", endpoint, nil)
	if e != nil {
		return
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2025-11-25")
	req.Header.Set("Mcp-Session-Id", session)
	res, e := client.Do(req)
	if e != nil {
		return
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return
	}
	reader := bufio.NewReaderSize(res.Body, 64<<10)
	var event []byte
	overflow, lineIsData, atLineStart := false, false, true
	dispatch := func() {
		defer func() { event, overflow = event[:0], false }()
		if overflow || len(event) == 0 {
			return
		}
		var msg struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
			Params  any             `json:"params,omitempty"`
		}
		if Decode(event, &msg) == nil && msg.JSONRPC == "2.0" && len(msg.ID) == 0 && msg.Method == "notifications/tools/list_changed" {
			g.markChanged(name)
			ev := Event{Kind: "list_changed", Server: name}
			if g.Safe(success(ev)).Status == 200 {
				g.opts.Event(ev)
			}
		}
	}
	for {
		fragment, err := reader.ReadSlice('\n')
		lineEnded := err == nil
		if err != nil && !errors.Is(err, bufio.ErrBufferFull) {
			return
		}
		if atLineStart {
			if lineEnded && len(bytes.TrimRight(fragment, "\r\n")) == 0 {
				dispatch()
				continue
			}
			field, rest, hasColon := bytes.Cut(fragment, []byte(":"))
			lineIsData = hasColon && string(field) == "data"
			if lineIsData {
				if len(rest) > 0 && rest[0] == ' ' {
					rest = rest[1:]
				}
				fragment = rest
				if len(event) > 0 && !overflow && len(event) < classifyWindow {
					event = append(event, '\n')
				}
			}
		}
		if lineIsData && !overflow {
			if lineEnded {
				fragment = bytes.TrimRight(fragment, "\r\n")
			}
			room := classifyWindow - len(event)
			if len(fragment) > room {
				event = append(event, fragment[:room]...)
				overflow = true
			} else {
				event = append(event, fragment...)
			}
		}
		atLineStart = lineEnded
	}
}
