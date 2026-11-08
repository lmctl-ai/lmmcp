#!/usr/bin/env python3
"""Production-copy controls for the wp357 review findings (the 11 surviving reviewer mutations plus the fixes).
A mutation counts as caught only when the copy compiles and a test fails; compile failures are reported apart.
usage: scripts/mutations-wp358.py --go GO --evidence DIR [--only name,name]   (set LMMCP_TEST_* to include integration)
"""
import argparse,json,os,pathlib,shutil,subprocess,tempfile
p=argparse.ArgumentParser();p.add_argument('--go',required=True);p.add_argument('--evidence',required=True);p.add_argument('--only',default='');a=p.parse_args()
root=pathlib.Path(__file__).resolve().parents[1];e=pathlib.Path(a.evidence).resolve();e.mkdir(parents=True,exist_ok=True)
R,G,A,C,J,CF,N,H,S,RE,M='internal/bridge/remote.go','internal/bridge/gateway.go','internal/bridge/api.go','internal/bridge/catalog.go','internal/bridge/json.go','internal/bridge/config.go','internal/bridge/notifications.go','internal/host/host.go','internal/bridge/address.go','internal/bridge/rpcerrors.go','cmd/lmmcp/main.go'
mutations=[
# the 11 reviewer survivors
('scheme-downgrade',[(R,'a.Scheme == b.Scheme && ','')]),
('redirects-followed',[(R,'CheckRedirect: func(*http.Request, []*http.Request) error { return bad("REDIRECT_REFUSED") }','CheckRedirect: nil')]),
('proxy-from-environment',[(R,'return &http.Transport{Proxy: nil,','return &http.Transport{Proxy: http.ProxyFromEnvironment,')]),
('remote-secrets-not-registered',[(R,'		g.addSecrets(secrets...)\n		r.headers.Set(k, value)','		_ = secrets\n		r.headers.Set(k, value)')]),
('reserved-session-id-header',[(R,'"transfer-encoding", "mcp-session-id", ','"transfer-encoding", ')]),
('duplicate-header-by-case',[(R,'if !headerName(k) || seen[key] {','if !headerName(k) {')]),
('url-query',[(R,'u.RawQuery != "" || strings.Contains(raw, "?") ||','')]),
('retry-ignores-trace',[(R,'	return t.sent.Load() == 1 && t.rejected.Load() == 1 && t.other.Load() == 0','	return true')]),
('delete-tied-to-lifetime',[(R,'ctx, life = context.WithoutCancel(ctx), r.abort','ctx, life = ctx, r.ctx')]),
('pagination-cycle-guard',[(C,'if seen[next] {','if false {')]),
('secret-key-scan',[(G,'if contains(k) || contains(a) {','if contains(a) || (false && contains(k)) {')]),
# the fixes
('retry-after-two-posts',[(R,'return t.sent.Load() == 1 && t.rejected','return t.rejected')]),
('retry-when-nothing-written-always',[(R,'	if t.sent.Load() == 0 {\n		return sessionDead\n	}','	if t.sent.Load() == 0 {\n		return true\n	}')]),
('replace-on-application-error',[(R,'if mapRPC(err, kind) == nil {','if true {')]),
('replace-under-running-calls',[(G,'	if e.users[session] > 0 {\n		e.retired[session]','	if false {\n		e.retired[session]')]),
('broken-session-kept',[(G,'if e.session != nil && e.broken == e.session {','if false {')]),
('delete-before-cancel-removed',[(G,'		remote.terminate(session)\n','')]),
('cancel-notice-bound-to-lifetime',[(R,'note := cancelNotification(req)','note := false')]),
('abort-does-nothing',[(G,'func (g *Gateway) Abort() { g.abortTeardown() }','func (g *Gateway) Abort() {}')]),
('rpc-resource-not-found',[(RE,'case code == codeResourceNotFound:','case false:')]),
('rpc-prompt-not-found',[(RE,'code == jsonrpc.CodeInvalidParams && kind == "prompt" && missing','false')]),
('rpc-invalid-params',[(RE,'		return rpcFailure(400, "INVALID_PARAMS", code)','		return rpcFailure(502, "UPSTREAM_ERROR", code)')]),
('rpc-local-transport-codes',[(RE,'if code == -32004 || code == -32005 {','if false {')]),
('call-status-not-recorded',[(R,'			trace.status.Store(int64(res.StatusCode))\n','')]),
('metadata-literal-allowed',[(S,'	return ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.Equal(metadataV6)','	return ip.IsUnspecified() || ip.Equal(metadataV6)')]),
('metadata-v6-allowed',[(S,' || ip.Equal(metadataV6)','')]),
('dial-time-check-removed',[(S,'if forbiddenAddress(ip) || !allowPrivate','if !allowPrivate')]),
('private-flag-ignored',[(R,'	return c.AllowPrivateAddresses == nil || *c.AllowPrivateAddresses\n','	return true\n')]),
('port-unchecked',[(R,'if err != nil || n < 1 || n > 65535 {','if err != nil {')]),
('userinfo-in-origin-hop',[(R,'req.URL.User != nil || !sameOrigin','!sameOrigin')]),
('default-port-not-normalised',[(R,'		if u.Scheme == "https" {\n			return "443"\n		}\n		return "80"','		return "0"')]),
('watch-window-unbounded',[(N,'const classifyWindow = 4096','const classifyWindow = 1 << 40')]),
('watch-overflow-ignored',[(N,'				overflow = true\n','')]),
('client-metadata-literal',[(M,' || ip != nil && bridge.ForbiddenAddress(ip)','')]),
('config-code-generic',[(M,'	if errors.As(e, &be) && be.Code != "" {','	if false {')]),
]
if a.only:mutations=[m for m in mutations if m[0] in a.only.split(',')]
results=[]
with tempfile.TemporaryDirectory(prefix='copy-',dir=e) as tmp:
 copy=pathlib.Path(tmp)
 for name in ['go.mod','go.sum']:shutil.copy2(root/name,copy/name)
 for name in ['cmd','internal']:shutil.copytree(root/name,copy/name)
 tags=['-tags','integration'] if os.environ.get('LMMCP_TEST_LMMOCK') else []
 for name,edits in mutations:
  originals={}
  try:
   for f,before,after in edits:
    path=copy/f;originals.setdefault(f,path.read_text());text=path.read_text()
    assert text.count(before)==1,(name,f,'anchor missing or ambiguous');path.write_text(text.replace(before,after,1))
   env=dict(os.environ,CGO_ENABLED='0')
   built=subprocess.run([a.go,'build','./...'],cwd=copy,env=env,capture_output=True,text=True)
   if built.returncode:
    results.append({'name':name,'caught':False,'compileError':True});(e/(name+'.log')).write_text(built.stdout+built.stderr);continue
   r=subprocess.run([a.go,'test','-count=1','-timeout=150s',*tags,'./...'],cwd=copy,env=env,text=True,capture_output=True,stdin=subprocess.DEVNULL)
   out=r.stdout+r.stderr
   results.append({'name':name,'caught':r.returncode!=0,'compileError':False,'failing':[l.split()[2] for l in out.splitlines() if l.startswith('--- FAIL')][:4]})
   (e/(name+'.log')).write_text(out)
  finally:
   for f,original in originals.items():(copy/f).write_text(original)
(e/'results.json').write_text(json.dumps(results,indent=2)+'\n')
bad=[x['name'] for x in results if not x['caught']]
print(json.dumps({'total':len(results),'caught':len(results)-len(bad),'notCaught':bad}))
raise SystemExit(1 if bad else 0)
