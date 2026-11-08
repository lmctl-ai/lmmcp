package bridge

import (
	"bufio"
	"net/http"
	"strings"
)

// The optional 2025 notification stream must not delay session initialization.
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
	reader := bufio.NewReader(res.Body)
	var data strings.Builder
	for {
		line, err := reader.ReadString('\n')
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if line == "" {
			if data.Len() > 0 {
				var msg struct {
					JSONRPC string `json:"jsonrpc"`
					Method  string `json:"method"`
					Params  any    `json:"params,omitempty"`
				}
				if Decode([]byte(data.String()), &msg) == nil && msg.JSONRPC == "2.0" && msg.Method == "notifications/tools/list_changed" {
					g.markChanged(name)
					event := Event{Kind: "list_changed", Server: name}
					if g.Safe(success(event)).Status == 200 {
						g.opts.Event(event)
					}
				}
				data.Reset()
			}
		}
		if field, value, ok := strings.Cut(line, ":"); ok && field == "data" {
			value = strings.TrimPrefix(value, " ")
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(value)
		}
		if err != nil {
			return
		}
	}
}
