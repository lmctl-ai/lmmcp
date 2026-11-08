"""Synthetic Streamable-HTTP (2025-11-25 JSON replies) MCP stub with hostile behaviours. Loopback only."""
import json,threading,time,sys,socket
from http.server import ThreadingHTTPServer,BaseHTTPRequestHandler
class State:
    def __init__(s):
        s.lock=threading.Lock();s.sessions=set();s.n=0;s.effects=0;s.log=[];s.cfg={}
        s.release=threading.Event();s.held=threading.Event()
ST=State()
TOOLS=[{"name":n,"description":n,"inputSchema":{"type":"object","properties":{}}} for n in ["effect","echo","slow","a/b","x.y","é","100%","..%2f"]]
class H(BaseHTTPRequestHandler):
    protocol_version='HTTP/1.1'
    def log_message(self,*a):pass
    def send_json(self,code,obj,extra=None):
        b=json.dumps(obj).encode();self.send_response(code);self.send_header('Content-Type','application/json');self.send_header('Content-Length',str(len(b)))
        for k,v in (extra or {}).items():self.send_header(k,v)
        self.end_headers();self.wfile.write(b)
    def do_GET(self):
        if self.path.startswith('/mcp') and ST.cfg.get('endless_get'):
            self.send_response(200);self.send_header('Content-Type','text/event-stream');self.send_header('Cache-Control','no-cache');self.end_headers()
            chunk=(b'data: '+b'A'*1048576+b'\n')
            try:
                while True:
                    self.wfile.write(chunk);self.wfile.flush()
            except Exception:return
        if self.path=='/ctl/state':
            with ST.lock:return self.send_json(200,{'effects':ST.effects,'log':ST.log,'sessions':sorted(ST.sessions)})
        self.send_response(405);self.send_header('Content-Length','0');self.end_headers()
    def do_DELETE(self):
        with ST.lock:ST.log.append({'method':'DELETE','path':self.path,'sid':self.headers.get('Mcp-Session-Id')})
        self.send_response(200);self.send_header('Content-Length','0');self.end_headers()
    def do_POST(self):
        n=int(self.headers.get('Content-Length','0'));body=self.rfile.read(n);path=self.path
        if path=='/ctl':
            with ST.lock:ST.cfg.update(json.loads(body));
            if ST.cfg.get('reset'):
                with ST.lock:ST.effects=0;ST.log=[];ST.cfg={};ST.release.clear();ST.held.clear()
            return self.send_json(200,{'ok':True})
        msg=json.loads(body);m=msg.get('method');i=msg.get('id');sid=self.headers.get('Mcp-Session-Id')
        with ST.lock:ST.log.append({'method':m,'path':path,'auth':self.headers.get('Authorization'),'host':self.headers.get('Host'),'sid':sid,'tool':(msg.get('params') or {}).get('name')})
        if m=='initialize':
            with ST.lock:ST.n+=1;s='S%d'%ST.n;ST.sessions.add(s)
            return self.send_json(200,{'jsonrpc':'2.0','id':i,'result':{'protocolVersion':'2025-11-25','capabilities':{'tools':{},'prompts':{},'resources':{}},'serverInfo':{'name':'stub','version':'1'}}},{'Mcp-Session-Id':s})
        if sid not in ST.sessions and m!='initialize':
            self.send_response(404);self.send_header('Content-Length','0');self.end_headers();return
        if i is None:
            self.send_response(202);self.send_header('Content-Length','0');self.end_headers();return
        if m=='tools/list':return self.send_json(200,{'jsonrpc':'2.0','id':i,'result':{'tools':TOOLS}})
        if m=='prompts/list':return self.send_json(200,{'jsonrpc':'2.0','id':i,'result':{'prompts':[]}})
        if m=='prompts/get':return self.send_json(200,{'jsonrpc':'2.0','id':i,'error':{'code':-32602,'message':'unknown prompt'}})
        if m=='resources/read':
            return self.send_json(200,{'jsonrpc':'2.0','id':i,'result':{'contents':[{'uri':msg['params']['uri'],'mimeType':'text/plain','text':'uri='+msg['params']['uri']}]}})
        if m=='tools/call':
            name=msg['params']['name']
            if name=='effect':
                with ST.lock:ST.effects+=1;first=ST.effects==1
                if ST.cfg.get('hold_first_effect') and first:
                    ST.held.set();ST.release.wait(20)
                return self.send_json(200,{'jsonrpc':'2.0','id':i,'result':{'content':[{'type':'text','text':'effect done'}]}})
            if name=='echo' and ST.cfg.get('expire_on_echo'):
                with ST.lock:ST.sessions.discard(sid);ST.cfg['expire_on_echo']=False
                self.send_response(404);self.send_header('Content-Length','0');self.end_headers();return
            if name=='slow':
                ST.held.set();ST.release.wait(float(ST.cfg.get('slow_secs',20)))
                return self.send_json(200,{'jsonrpc':'2.0','id':i,'result':{'content':[{'type':'text','text':'slow done'}]}})
            return self.send_json(200,{'jsonrpc':'2.0','id':i,'result':{'content':[{'type':'text','text':'tool='+name}]}})
        return self.send_json(200,{'jsonrpc':'2.0','id':i,'error':{'code':-32601,'message':'nope'}})
if __name__=='__main__':
    srv=ThreadingHTTPServer(('127.0.0.1',int(sys.argv[1]) if len(sys.argv)>1 else 0),H);srv.daemon_threads=True
    print(json.dumps({'url':'http://127.0.0.1:%d'%srv.server_address[1]}),flush=True);srv.serve_forever()
