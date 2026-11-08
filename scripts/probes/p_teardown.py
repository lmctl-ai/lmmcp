import sys,json,subprocess,tempfile;sys.path.insert(0,__import__('os').path.dirname(__import__('os').path.abspath(__file__)));from lib import *
sp,stub=start_stub();ctl(stub,{'reset':True})
d=tempfile.mkdtemp(prefix='wp357-');c=d+'/c.json';open(c,'w').write(json.dumps({'mcpServers':{'r':{'url':stub+'/mcp','type':'http','protocolVersion':'2025-11-25'}}}))
r=subprocess.run([BIN,'tools','r','--config',c],capture_output=True,text=True,env={'PATH':'/usr/bin:/bin','HOME':d})
st=state(stub);sp.terminate()
print('cli exit',r.returncode,'ok' if '"ok":true' in r.stdout else r.stdout[:80])
print('requests seen by server:',[(l['method'],l.get('sid')) for l in st['log']])
print('DELETE (session termination) sent:',any(l['method']=='DELETE' for l in st['log']))
