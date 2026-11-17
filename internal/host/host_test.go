package host

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	lmcode "github.com/lmctl-ai/lmcode-go"
	"github.com/lmctl-ai/lmmcp/internal/bridge"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestManifestBeforeDispatch(t *testing.T) {
	for _, raw := range []string{`{"version":1}`, `{"version":1,"dry":"false"}`, `{"version":2,"dry":false}`, `{"version":1,"dry":false,"dry":true}`} {
		p := filepath.Join(t.TempDir(), "manifest")
		os.WriteFile(p, []byte(raw), 0600)
		t.Setenv("LMCODE_INVOCATION_FILE", p)
		var out bytes.Buffer
		if code := Run([]string{"does-not-exist"}, context.Background(), nil, &out); code != 2 || !strings.Contains(out.String(), "LMCODE_RETURN") {
			t.Fatal(code, out.String())
		}
	}
}
func TestDryRegistryNoEffects(t *testing.T) {
	cfg := bridge.Config{Servers: map[string]bridge.ServerConfig{"r": {URL: "http://127.0.0.1:1", Headers: map[string]string{"Authorization": "${MUST_NOT_READ}"}}}}
	g, _ := bridge.New(cfg, bridge.Options{Lookup: func(string) (string, bool) { t.Fatal("dry credential access"); return "", false }})
	defer g.Close()
	r := Registry(context.Background(), g)
	f := r.FunctionBySignature("fn mcp_call(string, string, Json) -> Json")
	if f == nil || f.DryFn == nil {
		t.Fatal("missing dry slot")
	}
	v, e := f.DryFn([]lmcode.Value{lmcode.String("r"), lmcode.String("echo"), lmcode.None()})
	if e != nil || v.Obj == nil || g.StartCount() != 0 {
		t.Fatal(v, e)
	}
	m := r.MethodBySignature("method MCPServer.tools() -> Json")
	if _, e := m.Fn((*serverRef)(nil), nil); e == nil {
		t.Fatal("null receiver")
	}
}

func invocation(t *testing.T, dry bool, parts []map[string]any) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "invocation.json")
	mode := "single"
	if len(parts) > 1 {
		mode = "builder"
	}
	b, _ := json.Marshal(map[string]any{"version": 1, "mode": mode, "dry": dry, "parts": parts})
	os.WriteFile(p, b, 0600)
	t.Setenv("LMCODE_INVOCATION_FILE", p)
}
func TestDryProgramAndResultSink(t *testing.T) {
	file, _ := filepath.Abs("testdata/tools.lmcodeb")
	invocation(t, true, []map[string]any{{"index": 0, "file": file}})
	g, _ := bridge.New(bridge.Config{Servers: map[string]bridge.ServerConfig{"fixture": {Command: "must-not-start"}}}, bridge.Options{Lookup: func(string) (string, bool) { t.Fatal("dry lookup"); return "", false }})
	defer g.Close()
	var out bytes.Buffer
	if code := Run([]string{file}, context.Background(), g, &out); code != 0 || !strings.Contains(out.String(), `"action":"mcp_call"`) || !strings.Contains(out.String(), `"dry":true`) {
		t.Fatal(code, out.String())
	}
	if g.StartCount() != 0 {
		t.Fatal("dry effect")
	}
}
func TestBuilderPreloadAndOrder(t *testing.T) {
	file, _ := filepath.Abs("testdata/tools.lmcodeb")
	bad := filepath.Join(t.TempDir(), "bad")
	os.WriteFile(bad, []byte("invalid"), 0600)
	g, _ := bridge.New(bridge.Config{Servers: map[string]bridge.ServerConfig{"fixture": {Command: "must-not-start"}}}, bridge.Options{Lookup: func(string) (string, bool) { t.Error("activation before builder preload finished"); return "", false }})
	defer g.Close()
	invocation(t, false, []map[string]any{{"index": 0, "file": file}, {"index": 1, "file": bad}})
	var out bytes.Buffer
	if code := Run([]string{file, bad}, context.Background(), g, &out); code != 3 || g.StartCount() != 0 {
		t.Fatal(code, out.String())
	}
	copyFile := filepath.Join(t.TempDir(), "second.lmcode")
	raw, _ := os.ReadFile(file)
	os.WriteFile(copyFile, raw, 0600)
	invocation(t, true, []map[string]any{{"index": 0, "file": file}, {"index": 1, "file": copyFile}})
	out.Reset()
	if code := Run([]string{copyFile, file}, context.Background(), g, &out); code != 0 || !strings.Contains(out.String(), `"parts":2`) {
		t.Fatal(code, out.String())
	}
}
func TestManifestMetadataAdversarial(t *testing.T) {
	file, _ := filepath.Abs("testdata/tools.lmcodeb")
	for _, raw := range []string{`{"version":1,"mode":"single","parts":[]}`, `{"version":1,"mode":"single","dry":"true","parts":[]}`, `{"version":1,"mode":"builder","dry":false,"parts":[{"index":1e-9999,"file":"x"}]}`, `{"version":1,"mode":"builder","dry":false,"parts":[{"index":9007199254740992,"file":"x"}]}`, `{"version":1,"mode":"single","dry":false,"parts":[{"index":0,"index":0,"file":"x"}]}`} {
		p := filepath.Join(t.TempDir(), "manifest")
		os.WriteFile(p, []byte(raw), 0600)
		t.Setenv("LMCODE_INVOCATION_FILE", p)
		var out bytes.Buffer
		if code := Run([]string{file}, context.Background(), nil, &out); code != 2 {
			t.Fatal(code, out.String())
		}
	}
}
func TestSchemaFixtureMatches(t *testing.T) {
	b, e := os.ReadFile("../../examples/schema.json")
	if e != nil || !bytes.Equal(b, Schema) {
		t.Fatal("schema resource drift", e)
	}
}

func TestStdRestAndCliInterfaces(t *testing.T) {
	if os.Getenv("LMMCP_HOST_TEST_SUBPROC") == "1" {
		var files []string
		for i, a := range os.Args {
			if a == "--" {
				files = os.Args[i+1:]
				break
			}
		}
		g, _ := bridge.New(bridge.Config{Servers: map[string]bridge.ServerConfig{"fixture": {Command: "must-not-start"}}}, bridge.Options{})
		defer g.Close()
		os.Exit(Run(files, context.Background(), g, os.Stdout))
	}

	reg := Registry(context.Background(), nil)
	if got := len(reg.StdSignatures()); got != 226 {
		t.Fatalf("std signatures = %d, want 226", got)
	}

	lmscriptBin, err := exec.LookPath("lmscript")
	if err != nil {
		if mockBin := os.Getenv("LMMCP_TEST_LMMOCK"); mockBin != "" {
			candidate := filepath.Join(filepath.Dir(mockBin), "compiler", "node_modules", ".bin", "lmscript")
			if _, statErr := os.Stat(candidate); statErr == nil {
				lmscriptBin = candidate
			}
		}
	}
	if lmscriptBin == "" {
		t.Skip("lmscript not found on PATH")
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	schemaPath := filepath.Join(dir, "schema.json")
	if err := os.WriteFile(schemaPath, Schema, 0600); err != nil {
		t.Fatal(err)
	}
	lmsPath := filepath.Join(dir, "rest_cli.lms")
	lmsSource := fmt.Sprintf(`p = _lmcode_ schema=%q;
@"--server"
fn opt_server(x: RestExchange) -> void implements CliOption {
  x.set("server", x.query("server"));
}

@"tools"
fn cmd_tools(x: RestExchange) -> void implements CliCommand {
  let srv = mcp_server(x.get("server"));
  let res = srv.tools();
  x.send(res.get("error").get("code").as_string() + ":" + x.get("server"));
}

@"/call/:tool"
fn get_call(x: RestExchange) -> void implements RestGet {
  let servers = mcp_servers();
  let res = mcp_call(x.get("server"), x.param("tool"), json_parse("{\"text\":\"hello\"}"));
  x.json("{\"ok\":" + str(servers.get("ok").as_bool()) + ",\"code\":\"" + res.get("error").get("code").as_string() + "\",\"tool\":\"" + x.param("tool") + "\"}");
}

rest_config("session", "global");
let cli_out = rest_cli(str_split("--server=fixture tools", " "));
let rest_out = rest_cli(str_split("/call/echo", " "));
$return = json_parse("{\"cli\":\"" + cli_out + "\",\"rest\":" + rest_out + "}");
end _lmcode_
r = run_lmcode(p, {command: [%q, "-test.run=^TestStdRestAndCliInterfaces$", "--", "$DSLFILE"]});
require(r.ok, "std rest/cli host failed");
print(r.result);
`, filepath.ToSlash(schemaPath), filepath.ToSlash(exe))
	if err := os.WriteFile(lmsPath, []byte(lmsSource), 0600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(lmscriptBin, lmsPath)
	cmd.Env = append(os.Environ(), "LMMCP_HOST_TEST_SUBPROC=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("lmscript failed: %v\n%s", err, string(out))
	}
	if !strings.Contains(string(out), "START_FAILED:fixture") || !strings.Contains(string(out), `"tool":"echo"`) {
		t.Fatalf("unexpected output: %s", string(out))
	}
}
