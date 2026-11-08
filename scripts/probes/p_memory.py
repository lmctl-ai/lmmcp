import sys,time,json,os;sys.path.insert(0,__import__('os').path.dirname(__import__('os').path.abspath(__file__)));from lib import *
def rss(pid):
    for l in open('/proc/%d/status'%pid):
        if l.startswith('VmRSS'):return int(l.split()[1])//1024
sp,stub=start_stub();ctl(stub,{'reset':True});ctl(stub,{'endless_get':True})
g,base=serve({'mcpServers':{'r':{'url':stub+'/mcp','type':'http','protocolVersion':'2025-11-25'}}})
res={}
try:
    r0=rss(g.pid);t=call(base,'/v1/servers/r/tools',timeout=20)
    samples=[]
    for i in range(6):
        time.sleep(1);samples.append(rss(g.pid))
    h=call(base,'/health',timeout=5)
    res={'rssBeforeMB':r0,'toolsStatus':t[0],'rssMBEachSecond':samples,'healthAlive':h[0]}
    # cancel path: closing the session must stop the reader
finally:
    g.terminate();g.wait();sp.terminate()
print(json.dumps(res));
