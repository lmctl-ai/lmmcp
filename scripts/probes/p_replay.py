import sys,threading,time,json;sys.path.insert(0,__import__('os').path.dirname(__import__('os').path.abspath(__file__)));from lib import *
sp,stub=start_stub();ctl(stub,{'reset':True})
g,base=serve({'mcpServers':{'r':{'url':stub+'/mcp','type':'http','protocolVersion':'2025-11-25'}}})
res={}
try:
    call(base,'/v1/servers/r/tools')
    ctl(stub,{'hold_first_effect':True,'expire_on_echo':True})
    out={}
    ta=threading.Thread(target=lambda:out.update(A=call(base,'/v1/servers/r/tools/effect/call','POST',{})));ta.start()
    for _ in range(200):
        if state(stub)['effects']>=1:break
        time.sleep(0.05)
    res['effectsBeforeB']=state(stub)['effects']
    out['B']=call(base,'/v1/servers/r/tools/echo/call','POST',{})
    ta.join(40)
    st=state(stub)
    res.update({'A':out.get('A'),'B':out.get('B'),'effectExecutions':st['effects'],'serverSawToolCalls':[l['tool'] for l in st['log'] if l['method']=='tools/call']})
finally:
    g.terminate();sp.terminate()
print(json.dumps(res,indent=1))
