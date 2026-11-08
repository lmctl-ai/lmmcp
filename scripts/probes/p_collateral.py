import sys,threading,time,json;sys.path.insert(0,__import__('os').path.dirname(__import__('os').path.abspath(__file__)));from lib import *
sp,stub=start_stub();ctl(stub,{'reset':True});ctl(stub,{'slow_secs':6})
g,base=serve({'mcpServers':{'r':{'url':stub+'/mcp','type':'http','protocolVersion':'2025-11-25'}}})
res={}
try:
    call(base,'/v1/servers/r/tools')
    out={}
    t=threading.Thread(target=lambda:out.update(slow=call(base,'/v1/servers/r/tools/slow/call','POST',{}),at=time.time()));t0=time.time();t.start()
    time.sleep(1.0)
    p=call(base,'/v1/servers/r/prompts/nonexistent','POST',{})
    t.join(30)
    inits=[l for l in state(stub)['log'] if l['method']=='initialize']
    res={'promptError':p,'slowResult':out['slow'],'slowReturnedAfterSecs':round(out['at']-t0,1),'initializeCount':len(inits)}
finally:
    g.terminate();sp.terminate()
print(json.dumps(res,indent=1))
