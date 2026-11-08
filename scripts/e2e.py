#!/usr/bin/env python3
"""Actual CLI/HTTP/JSON-lines integration; synthetic env and loopback only."""
import argparse,json,os,pathlib,subprocess,urllib.request,threading,signal
p=argparse.ArgumentParser();p.add_argument('--binary',required=True);p.add_argument('--mock',required=True);p.add_argument('--evidence',required=True);a=p.parse_args();e=pathlib.Path(a.evidence).resolve();e.mkdir(parents=True,exist_ok=False)
secret='SYNTHETIC_ONLY_SECRET_350';token='SYNTHETIC_ONLY_BEARER_350';pidfile=e/'child.pid';cfg=e/'config.json';cfg.write_text(json.dumps({'mcpServers':{'fixture':{'command':a.mock,'args':['-test.run=TestMockProcess'],'env':{'LMMCP_TEST_SERVER':'1','TEST_TOKEN':'${FIXTURE_SECRET}','LMMCP_PID_FILE':str(pidfile)}}}}));env={'PATH':os.environ['PATH'],'HOME':str(e),'FIXTURE_SECRET':secret,'BRIDGE_AUTH':token}
def call(args):
 r=subprocess.run([a.binary]+args,env=env,capture_output=True,text=True);assert secret not in r.stdout+r.stderr and token not in r.stdout+r.stderr;assert r.returncode==0,(r.returncode,r.stdout,r.stderr);return json.loads(r.stdout)
assert call(['servers','--config',str(cfg)])['ok'];assert not pidfile.exists()
assert len(call(['tools','fixture','--config',str(cfg)])['data'])>=7
assert call(['call','fixture','echo','--arg','value=in-process','--config',str(cfg)])['data']['content'][0]['text']=='in-process'
lines='\n'.join(json.dumps(x) for x in [{'id':1,'method':'GET','path':'/v1/servers'},{'id':2,'method':'POST','path':'/v1/servers/fixture/tools/echo/call','body':{'value':'stdio'}}])+'\n';r=subprocess.run([a.binary,'stdio','--config',str(cfg)],input=lines,env=env,capture_output=True,text=True);rows=[json.loads(x) for x in r.stdout.splitlines()];assert r.returncode==0 and len(rows)==2 and {x['id'] for x in rows}=={1,2};assert secret not in r.stdout+r.stderr and token not in r.stdout+r.stderr
proc=subprocess.Popen([a.binary,'serve','--listen','127.0.0.1:0','--config',str(cfg),'--token-env','BRIDGE_AUTH'],env=env,stdout=subprocess.PIPE,stderr=subprocess.PIPE,text=True)
def identity(pid):return pathlib.Path(f'/proc/{pid}/stat').read_text().rsplit(')',1)[1].split()[19]
start=identity(proc.pid);url=json.loads(proc.stdout.readline())['url'];events=[];progress=threading.Event()
def logs():
 for line in proc.stderr:
  assert secret not in line and token not in line
  event=json.loads(line);events.append(event)
  if event.get('event')=='progress':progress.set()
thread=threading.Thread(target=logs);thread.start()
try:
 try:urllib.request.urlopen(url+'/v1/servers')
 except urllib.error.HTTPError as error:assert error.code==401
 else:raise AssertionError('unauthenticated access')
 assert call(['call','fixture','echo','--json','{"value":"http-client"}','--url',url,'--token-env','BRIDGE_AUTH'])['data']['content'][0]['text']=='http-client'
 request=urllib.request.Request(url+'/v1/openapi.json',headers={'Authorization':'Bearer '+token});assert json.load(urllib.request.urlopen(request))==call(['openapi'])
 slow=subprocess.Popen([a.binary,'call','fixture','slow','--json','{}','--url',url,'--token-env','BRIDGE_AUTH'],env=env,stdout=subprocess.PIPE,stderr=subprocess.PIPE,text=True);slow_start=identity(slow.pid);progress.wait();assert identity(slow.pid)==slow_start;slow.send_signal(signal.SIGINT);out,err=slow.communicate();assert slow.returncode==4 and json.loads(out)['error']['code']=='CANCELLED';assert secret not in out+err and token not in out+err
 # A later call still uses the same server and succeeds after cancellation.
 assert call(['call','fixture','echo','--arg','value=after-cancel','--url',url,'--token-env','BRIDGE_AUTH'])['ok']
 child=int(pidfile.read_text());child_start=identity(child)
finally:
 assert identity(proc.pid)==start;proc.send_signal(signal.SIGINT);proc.wait();thread.join()
assert proc.returncode==4
try:assert identity(child)!=child_start
except FileNotFoundError:pass
assert sum(x.get('event')=='started' for x in events)==1
result={'inProcess':True,'jsonLines':True,'httpClient':True,'bearer':True,'openapiMatches':True,'ctrlCCancel':True,'sessionReused':True,'childGone':True,'bridgePID':proc.pid,'bridgeStart':start,'childPID':child,'childStart':child_start,'signals':'SIGINT only to the two processes created and start-identity checked by this harness'};(e/'results.json').write_text(json.dumps(result,indent=2)+'\n');print(json.dumps(result))
