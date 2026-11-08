package bridge

import (
	"errors"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"strings"
)

// codeResourceNotFound is the MCP specification's code for a missing resource.
const codeResourceNotFound = -32002

func rpcFailure(status int, code string, mcpCode int) *Error {
	e := bad(code)
	e.status = status
	e.MCPCode = &mcpCode
	return e
}

func isMissingEntityMessage(raw string) bool {
	words := strings.FieldsFunc(strings.ToLower(raw), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	})
	for i, w := range words {
		if w == "not" && i+1 < len(words) && words[i+1] == "found" {
			return true
		}
		if w == "unknown" {
			if i+1 < len(words) {
				switch words[i+1] {
				case "argument", "arguments", "parameter", "parameters", "field", "fields", "option", "options":
					continue
				}
			}
			return true
		}
	}
	return false
}

// mapRPC turns a JSON-RPC error answered by an MCP server into a typed REST error. The upstream message is never
// copied: only the code, so a server cannot echo text (or a credential) through it. kind is the operation family.
func mapRPC(err error, kind string) *Error {
	var we *jsonrpc.Error
	if !errors.As(err, &we) {
		return nil
	}
	code := int(we.Code)
	if code == -32004 || code == -32005 {
		return nil // the SDK's own "connection is closing" / "rejected by transport": a local transport failure
	}
	missing := isMissingEntityMessage(we.Message)
	switch {
	case code == codeResourceNotFound:
		return rpcFailure(404, "RESOURCE_NOT_FOUND", code)
	case code == jsonrpc.CodeMethodNotFound:
		return rpcFailure(404, "METHOD_NOT_SUPPORTED", code)
	case code == jsonrpc.CodeInvalidParams && kind == "prompt" && missing:
		return rpcFailure(404, "PROMPT_NOT_FOUND", code)
	case code == jsonrpc.CodeInvalidParams && kind == "resource" && missing:
		return rpcFailure(404, "RESOURCE_NOT_FOUND", code)
	case code == jsonrpc.CodeInvalidParams:
		return rpcFailure(400, "INVALID_PARAMS", code)
	case code == jsonrpc.CodeInternalError:
		return rpcFailure(502, "UPSTREAM_INTERNAL_ERROR", code)
	}
	return rpcFailure(502, "UPSTREAM_ERROR", code)
}

// classify names a failed call. JSON-RPC errors keep their MCP meaning; transport failures are named from this call's
// own trace, not from anything another call saw.
func classify(err error, r *remoteState, trace *callTrace, kind string) error {
	if e := mapRPC(err, kind); e != nil {
		return e
	}
	if r != nil {
		if r.refused.Load() {
			return bad("ADDRESS_REFUSED")
		}
		switch trace.status.Load() {
		case 401:
			return bad("UPSTREAM_UNAUTHORIZED")
		case 403:
			return bad("UPSTREAM_FORBIDDEN")
		}
		if errors.Is(err, mcp.ErrSessionMissing) {
			return bad("SESSION_EXPIRED")
		}
	}
	return bad("UPSTREAM_FAILED")
}
