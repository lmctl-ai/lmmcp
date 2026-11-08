#!/usr/bin/env python3
"""Production-copy controls; assertion failures count, not syntax/import/setup errors."""
import argparse,json,pathlib,shutil,subprocess,tempfile
p=argparse.ArgumentParser();p.add_argument('--go',required=True);p.add_argument('--evidence',required=True);a=p.parse_args();root=pathlib.Path(__file__).resolve().parents[1];e=pathlib.Path(a.evidence).resolve();e.mkdir(parents=True,exist_ok=True)
mutations=[
('sse-ceiling','internal/bridge/remote.go','MaxEventSize: -1, DisableStandaloneSSE','MaxEventSize: 0, DisableStandaloneSSE','TestRemoteLargeEvent'),
('auto-fallback','internal/bridge/remote.go','err != nil && kind == "auto"','err != nil && false','TestReleasedAutoAndAuth|TestAutoFallsBackToLegacySSEOnlyOnNonAuth4xx'),
('header-references','internal/bridge/remote.go','!strings.Contains(v, "${")','false','TestRemoteConfigPolicy'),
('plain-http-policy','internal/bridge/remote.go','!ip.IsLoopback()','false','TestRemoteConfigPolicy'),
('header-controls','internal/bridge/remote.go','strings.ContainsAny(value, "\\r\\n\\x00")','false','TestRemoteHeaderBeforeNetwork'),
('header-auth-forward','internal/bridge/remote.go','for k, v := range r.headers {','for k, v := range r.headers { if k=="Authorization"{continue}','TestRemoteAuthAndNoRedirect'),
('origin-hop','internal/bridge/remote.go','req.URL.User != nil || !sameOrigin(req.URL, r.origin)','false','TestRemoteOriginHop'),
('retry-once-bound','internal/bridge/remote.go','attempt < 2','attempt < 1','TestRemoteRetryOnce'),
('session-retry','internal/bridge/remote.go','resend := r != nil && trace.mayResend(dead)','resend := r != nil || dead','TestEffectCallNeverResent|TestNoResendAfterWrittenRequest'),
('active-generation','internal/bridge/gateway.go','e.users[e.session] == 0 && e.changed.Swap(false)','e.changed.Swap(false)','TestRemoteChangeKeepsActiveCall'),
('list-refresh','internal/bridge/gateway.go','e.changed.Swap(false)','false','TestReleasedCancelAndChanged'),
('resource-route','internal/bridge/api.go','return g.catalog(ctx, p[2], "resources")','return failure(501,"NOT_IMPLEMENTED")','TestRemoteSurfaces'),
('template-route','internal/bridge/api.go','return g.catalog(ctx, p[2], "templates")','return failure(501,"NOT_IMPLEMENTED")','TestRemoteSurfaces'),
('prompt-object','internal/bridge/api.go','var raw map[string]json.RawMessage // every value must be a JSON string: null and other types are refused\n\t\t\tif Decode(r.Body, &raw) != nil || raw == nil {','var raw map[string]json.RawMessage\n\t\t\tif false {','TestCatalogInvalidArguments|TestPromptArgumentsMustBeAStringObject'),
('uri-required','internal/bridge/catalog.go','if uri == "" {','if false {','TestCatalogInvalidArguments'),
('structured-output','internal/bridge/gateway.go','if res.IsError {','res.StructuredContent=nil; if res.IsError {','TestRemoteSurfaces'),
('tool-http-status','internal/bridge/gateway.go','failure(200, "TOOL_ERROR")','failure(502, "TOOL_ERROR")','TestRemoteSurfaces'),
('tool-cli-exit','internal/bridge/api.go','m["ok"] == false','m["ok"] == true','TestRemoteSurfaces'),
('host-dry','internal/host/host.go','vmctx.SetDry(results.Dry())','vmctx.SetDry(false)','TestDryProgramAndResultSink'),
('null-receiver','internal/host/host.go','return lmcode.None(), lmcode.NewHostError("null_receiver")','return jsonValue(map[string]any{})','TestDryRegistryNoEffects'),
('builder-preload','internal/host/host.go','programs[index] = p','programs[index] = p; if !results.Dry() && g!=nil {g.Dispatch(ctx,bridge.Request{Method:"GET",Path:"/v1/servers/fixture/tools"})}','TestBuilderPreloadAndOrder'),
('manifest-before-dispatch','internal/host/host.go','return lmcode.FailureFrame(out, "cli", err)','return 0','TestManifestBeforeDispatch'),
]
results=[]
with tempfile.TemporaryDirectory(prefix='copy-',dir=e) as tmp:
 copy=pathlib.Path(tmp)
 for name in ['go.mod','go.sum']:shutil.copy2(root/name,copy/name)
 for name in ['cmd','internal','examples']:shutil.copytree(root/name,copy/name)
 for name,file,before,after,test in mutations:
  f=copy/file;original=f.read_text();assert before in original,(name,before);f.write_text(original.replace(before,after,1))
  try:
   r=subprocess.run([a.go,'test','-tags','integration','./...','-run',test,'-count=1','-timeout=45s'],cwd=copy,text=True,capture_output=True)
   output=r.stdout+r.stderr
   caught=r.returncode!=0 and '--- FAIL:' in output and '[build failed]' not in output and 'panic:' not in output
   results.append({'name':name,'test':test,'caught':caught});(e/(name+'.log')).write_text(output)
  finally:f.write_text(original)
(e/'results.json').write_text(json.dumps(results,indent=2)+'\n');print(json.dumps({'caught':sum(x['caught'] for x in results),'total':len(results),'survivors':[x['name'] for x in results if not x['caught']]}))
raise SystemExit(0 if all(x['caught'] for x in results) else 1)
