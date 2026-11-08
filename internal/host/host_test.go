package host

import (
	"bytes"
	"context"
	"encoding/json"
	lmcode "github.com/lmctl-ai/lmcode-go"
	"github.com/lmctlhq/lmmcp/internal/bridge"
	"os"
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
