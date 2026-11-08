#!/usr/bin/env python3
"""Production-copy controls; assertion failures count, not syntax/import/setup errors."""
import argparse,json,pathlib,shutil,subprocess,tempfile
p=argparse.ArgumentParser();p.add_argument('--go',required=True);p.add_argument('--evidence',required=True);a=p.parse_args();root=pathlib.Path(__file__).resolve().parents[1];e=pathlib.Path(a.evidence).resolve();e.mkdir(parents=True,exist_ok=True)
mutations=[
('frame-ceiling','internal/bridge/gateway.go','MaxLineLength: -1','MaxLineLength: 0','TestLargeNoImplicitCap'),
('read-only','internal/bridge/gateway.go','g.opts.ReadOnly &&','false &&','TestPolicy'),
('deny','internal/bridge/gateway.go','has(c.DenyTools, tool)','false','TestPolicy'),
('allow-empty','internal/bridge/gateway.go','c.AllowTools != nil && !has(c.AllowTools, tool)','false','TestAllowAndHostPolicy'),
('auth','internal/bridge/api.go','subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1','subtle.ConstantTimeCompare([]byte(token),[]byte(token)) != 0','TestHTTPAuth'),
('origin','internal/bridge/api.go','r.Header.Get("Origin") != ""','false','TestHTTPAuth'),
('host','internal/bridge/api.go','r.Host != host','false','TestAllowAndHostPolicy'),
('json-duplicate','internal/bridge/json.go','!ok || f.seen[k]','!ok','TestConfig'),
('argument-shape','internal/bridge/api.go','Decode(r.Body, &args) != nil || args == nil','false','TestErrorsAndSecret'),
('env-default','internal/bridge/gateway.go','inherited = []string{"PATH"}','inherited = []string{"PATH","UNDECLARED"}','TestOnlyDeclaredEnvIsRead'),
('tool-error','internal/bridge/gateway.go','if res.IsError {','if false {','TestRemoteSurfaces'),
('secret-output','internal/bridge/gateway.go','if contains(data) {','if false {','TestErrorsAndSecret'),
('child-wait','internal/bridge/gateway.go','_ = c.command.Wait()','_ = error(nil)','TestOwnedShutdownWait'),
('unknown-tool','internal/bridge/gateway.go','if selected == nil {','if true {','TestLazyDiscoveryCall'),

('missing-env','internal/bridge/config.go','if !ok {','if !ok && ok {','TestMissingEnvNoChild'),
('config-unknown','internal/bridge/json.go','d.DisallowUnknownFields()','// deliberately removed','TestConfig'),
('error-exit','internal/bridge/api.go','return 3','return 0','TestAPIContractEdges'),
('cancel-exit','internal/bridge/api.go','return 4','return 0','TestAPIContractEdges'),
('path-query','internal/bridge/api.go','strings.ContainsAny(path, "?#")','false','TestAPIContractEdges'),
]
results=[]
with tempfile.TemporaryDirectory(prefix='copy-',dir=e) as tmp:
 copy=pathlib.Path(tmp)
 for name in ['go.mod','go.sum']:shutil.copy2(root/name,copy/name)
 for name in ['cmd','internal','examples']:shutil.copytree(root/name,copy/name)
 for name,file,before,after,test in mutations:
  f=copy/file;original=f.read_text();assert before in original,(name,before);f.write_text(original.replace(before,after,1))
  try:
   r=subprocess.run([a.go,'test','./...','-run',test,'-count=1'],cwd=copy,text=True,capture_output=True)
   output=r.stdout+r.stderr
   caught=r.returncode!=0 and '--- FAIL:' in output and '[build failed]' not in output and 'panic:' not in output
   results.append({'name':name,'test':test,'caught':caught});(e/(name+'.log')).write_text(output)
  finally:f.write_text(original)
(e/'results.json').write_text(json.dumps(results,indent=2)+'\n');print(json.dumps({'caught':sum(x['caught'] for x in results),'total':len(results),'survivors':[x['name'] for x in results if not x['caught']]}))
raise SystemExit(0 if all(x['caught'] for x in results) else 1)
