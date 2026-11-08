package host

import (
	"context"
	"encoding/json"
	lmcode "github.com/lmctl-ai/lmcode-go"
	"github.com/lmctl-ai/lmmcp/internal/bridge"
	"io"
	"net/url"
	"os"
	"strings"
)

type spec struct {
	name   string
	params []string
	method string
}

var specs = []spec{{"mcp_servers", []string{}, ""}, {"mcp_tools", []string{"string"}, "tools"}, {"mcp_call", []string{"string", "string", "Json"}, "call"}, {"mcp_resources", []string{"string"}, "resources"}, {"mcp_templates", []string{"string"}, "templates"}, {"mcp_read", []string{"string", "string"}, "read"}, {"mcp_prompts", []string{"string"}, "prompts"}, {"mcp_prompt", []string{"string", "string", "Json"}, "prompt"}}
var Schema = makeSchema()

func makeSchema() []byte {
	functions := map[string]any{}
	methods := map[string]any{}
	signature := func(p []string, r string) any { return []any{map[string]any{"params": p, "returns": r}} }
	for _, s := range specs {
		functions[s.name] = signature(s.params, "Json")
		if s.method != "" {
			methods[s.method] = signature(s.params[1:], "Json")
		}
	}
	functions["mcp_server"] = signature([]string{"string"}, "MCPServer")
	b, _ := json.Marshal(map[string]any{"dsl.schema": 2, "name": "lmmcp", "types": map[string]any{"MCPServer": map[string]any{"methods": methods}}, "functions": functions, "result": map[string]any{"type": "Json", "nullable": true}})
	return append(b, '\n')
}
func jsonValue(value any) (lmcode.Value, error) {
	b, e := json.Marshal(value)
	if e != nil {
		return lmcode.None(), lmcode.NewHostError("serialization_failed")
	}
	j, e := lmcode.ParseJSON(string(b))
	if e != nil {
		return lmcode.None(), lmcode.NewHostError("serialization_failed")
	}
	return lmcode.Object("Json", j), nil
}

type serverRef struct{ name string }

func dispatch(ctx context.Context, g *bridge.Gateway, s spec, a []lmcode.Value) (lmcode.Value, error) {
	r := bridge.Request{Method: "GET", Path: "/v1/servers"}
	if len(a) > 0 {
		r.Path += "/" + url.PathEscape(a[0].Str)
	}
	switch s.method {
	case "tools", "resources", "prompts":
		r.Path += "/" + s.method
	case "templates":
		r.Path += "/resources/templates"
	case "read":
		r.Path += "/resources/read"
		r.Query = map[string]string{"uri": a[1].Str}
	case "call", "prompt":
		r.Method = "POST"
		if s.method == "call" {
			r.Path += "/tools/" + url.PathEscape(a[1].Str) + "/call"
		} else {
			r.Path += "/prompts/" + url.PathEscape(a[1].Str)
		}
		j, ok := a[2].Obj.(*lmcode.JSON)
		if !ok || j == nil {
			return lmcode.None(), lmcode.NewHostError("invalid_arguments")
		}
		r.Body = json.RawMessage(j.ToJSON())
	}
	result := g.Safe(g.Dispatch(ctx, r))
	if ctx.Err() != nil {
		return lmcode.None(), lmcode.NewHostError("cancelled")
	}
	return jsonValue(result.Body)
}
func Registry(ctx context.Context, g *bridge.Gateway) *lmcode.Registry {
	r := lmcode.NewRegistry()
	for _, item := range specs {
		s := item
		fn := lmcode.Function(func(a []lmcode.Value) (lmcode.Value, error) {
			if g == nil {
				return lmcode.None(), lmcode.NewHostError("missing_gateway")
			}
			return dispatch(ctx, g, s, a)
		})
		dry := lmcode.Function(func([]lmcode.Value) (lmcode.Value, error) {
			return jsonValue(map[string]any{"ok": true, "dry": true, "action": s.name, "started": false})
		})
		_ = r.Function(s.name, s.params, "Json", fn, lmcode.Dry(dry))
		if s.method != "" {
			method := lmcode.Method(func(self any, a []lmcode.Value) (lmcode.Value, error) {
				ref, ok := self.(*serverRef)
				if !ok || ref == nil {
					return lmcode.None(), lmcode.NewHostError("null_receiver")
				}
				return fn(append([]lmcode.Value{lmcode.String(ref.name)}, a...))
			})
			preview := lmcode.Method(func(any, []lmcode.Value) (lmcode.Value, error) { return dry(nil) })
			_ = r.Method("MCPServer", s.method, s.params[1:], "Json", method, lmcode.Dry(preview))
		}
	}
	acquire := lmcode.Function(func(a []lmcode.Value) (lmcode.Value, error) {
		return lmcode.Object("MCPServer", &serverRef{name: a[0].Str}), nil
	})
	_ = r.Function("mcp_server", []string{"string"}, "MCPServer", acquire, lmcode.Dry(acquire))
	return r
}
func Run(files []string, ctx context.Context, g *bridge.Gateway, out io.Writer) int {
	results, err := lmcode.InvocationResultsFromCommand(files)
	if err != nil {
		return lmcode.FailureFrame(out, "cli", err)
	}
	reg := Registry(ctx, g)
	contract, err := lmcode.ResultContractFromSchema(Schema)
	if err != nil {
		return lmcode.FailureFrame(out, "app", lmcode.NewHostError("invalid_schema"))
	}
	slots := results.Registrations()
	programs := make([]*lmcode.Program, len(slots))
	for _, slot := range slots {
		index := slot.Index
		raw, e := os.ReadFile(slot.File)
		if e != nil {
			_ = results.Fail(&index, "cli", lmcode.NewError(lmcode.CLI_USAGE, "program_unreadable"))
			return results.WriteFrame(out)
		}
		p, e := lmcode.Load(raw, Schema, reg)
		if e != nil {
			_ = results.Fail(&index, "load", lmcode.NewError(lmcode.LOAD_BAD_BYTECODE, "invalid_program"))
			return results.WriteFrame(out)
		}
		programs[index] = p
	}
	for i, p := range programs {
		if _, err = results.Select(i, reg, contract); err != nil {
			_ = results.Fail(&i, "app", err)
			break
		}
		vmctx := lmcode.NewContext()
		vmctx.SetDry(results.Dry())
		if err = p.Run(vmctx, reg); err != nil {
			phase := "run"
			if e, ok := err.(*lmcode.Error); ok && e.Code == lmcode.RUN_HOST {
				phase = "app"
			}
			_ = results.Fail(&i, phase, err)
			break
		}
		if err = results.Complete(i); err != nil {
			_ = results.Fail(&i, "app", err)
			break
		}
	}
	return results.WriteFrame(out)
}

// Fail reports a failure before any program ran as a terminal frame with exit 2.
func Fail(out io.Writer, code string) int {
	return lmcode.FailureFrame(out, "cli", lmcode.NewError(lmcode.CLI_USAGE, code))
}
func Signatures(ctx context.Context) string {
	return strings.Join(Registry(ctx, nil).Signatures(), "\n") + "\n"
}
