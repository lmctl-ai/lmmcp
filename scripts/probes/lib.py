import json,os,subprocess,sys,tempfile,threading,time,urllib.request,urllib.error
import pathlib
HERE=str(pathlib.Path(__file__).resolve().parent);BIN=os.environ['LMMCP_BIN']  # the lmmcp binary under test
def start_stub():
    p=subprocess.Popen([sys.executable,'-I',HERE+'/stub.py'],stdout=subprocess.PIPE,stderr=subprocess.DEVNULL)
    return p,json.loads(p.stdout.readline())['url']
def ctl(base,obj):
    r=urllib.request.Request(base+'/ctl',data=json.dumps(obj).encode(),headers={'Content-Type':'application/json'});urllib.request.urlopen(r).read()
def state(base):return json.load(urllib.request.urlopen(base+'/ctl/state'))
def serve(cfg,env=None):
    d=tempfile.mkdtemp(prefix='wp357-');c=d+'/c.json';open(c,'w').write(json.dumps(cfg))
    p=subprocess.Popen([BIN,'serve','--config',c,'--listen','127.0.0.1:0'],stdout=subprocess.PIPE,stderr=subprocess.PIPE,env=dict({'PATH':'/usr/bin:/bin','HOME':d},**(env or {})))
    # serve prints the actual URL
    line=p.stdout.readline();return p,json.loads(line)['url']
def call(base,path,method='GET',body=None,headers=None,timeout=60):
    h={'Content-Type':'application/json'};h.update(headers or {})
    r=urllib.request.Request(base+path,data=None if body is None else json.dumps(body).encode(),method=method,headers=h)
    try:
        x=urllib.request.urlopen(r,timeout=timeout);return x.status,json.loads(x.read())
    except urllib.error.HTTPError as e:return e.code,json.loads(e.read())
