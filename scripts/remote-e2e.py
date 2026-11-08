#!/usr/bin/env python3
"""Run under the digest-verified lmmock exec; no real MCP endpoints or secrets."""
import argparse,json,os,pathlib,subprocess,tempfile,urllib.request
p=argparse.ArgumentParser();p.add_argument('--binary',required=True);p.add_argument('--lmscript',required=True);p.add_argument('--mode',choices=['json','sse'],default='json');p.add_argument('--version',default='2026-07-28');a=p.parse_args();base=os.environ['LMMOCK_URL'];root=pathlib.Path(__file__).resolve().parents[1]
with urllib.request.urlopen(urllib.request.Request(base+'/__mock/reply-mode',data=json.dumps({'mode':a.mode}).encode(),headers={'Content-Type':'application/json'})) as r:assert r.status==200
with tempfile.TemporaryDirectory(prefix='lmmcp-script-') as tmp:
 d=pathlib.Path(tmp);config=d/'mcp.json';config.write_text(json.dumps({'mcpServers':{'fixture':{'url':base+('/sse' if a.version=='2024-11-05' else '/mcp'),'type':'sse' if a.version=='2024-11-05' else 'http','protocolVersion':a.version}}}))
 env={'PATH':str(pathlib.Path(a.binary).parent)+os.pathsep+os.environ['PATH'],'HOME':tmp}
 def cli(*args,code=0):
  r=subprocess.run([a.binary,*args,'--config',str(config)],env=env,text=True,capture_output=True);assert r.returncode==code,(r.returncode,r.stdout,r.stderr);return json.loads(r.stdout)
 assert cli('servers')['ok'];assert len(cli('tools','fixture')['data'])==6
 added=cli('call','fixture','add','--json','{"left":2,"right":4}')['data']
 assert (added['content'][0]['text'].strip() in ('6','6.0') if a.version=='2024-11-05' else added['structuredContent']['sum']==6)  # 2024-11-05 has no structuredContent
 assert cli('call','fixture','fail','--json','{}',code=3)['data']['isError']
 assert len(cli('resources','fixture')['data'])==2
 assert cli('read','fixture','mock://blob')['data']['contents'][0]['blob']=='AAEC'
 assert cli('templates','fixture')['data'][0]['uriTemplate']=='mock://item/{id}'
 assert cli('prompts','fixture')['data'][0]['name']=='greet'
 assert cli('prompt','fixture','greet','--arg','name=Ada')['data']['messages'][0]['content']['text']=='Hello Ada'
 script=root/'examples/tools.lms'
 for args in [['--check',str(script)],[str(script),str(config)]]:
  r=subprocess.run([a.lmscript,*args],env=env,text=True,capture_output=True);assert r.returncode==0,(r.returncode,r.stdout,r.stderr)
  if args[0]!='--check':assert 'hello from lmcode' in r.stdout,r.stdout
 print(json.dumps({'transport':a.version,'reply':a.mode,'cli':True,'lmscriptCheck':True,'runLmcode':True}))
