#!/usr/bin/env python3
"""Run public CI locally: mocks only, all logs retained, no releases or credentials."""
import argparse,hashlib,json,os,pathlib,subprocess,sys
ROOT=pathlib.Path(__file__).resolve().parents[1]
TARGETS=[('linux','amd64'),('linux','arm64'),('darwin','amd64'),('darwin','arm64'),('windows','amd64')]

def main():
 parser=argparse.ArgumentParser(description=__doc__);parser.add_argument('--go',default='go');parser.add_argument('--cc',default='gcc');parser.add_argument('--mock-prefix',required=True,type=pathlib.Path);parser.add_argument('--evidence',required=True,type=pathlib.Path);parser.add_argument('--skip-prepare',action='store_true');parser.add_argument('--skip-mutations',action='store_true',help='local repeat only: mutation suites were already run separately');args=parser.parse_args()
 evidence=args.evidence.resolve();mock=args.mock_prefix.resolve();evidence.mkdir(parents=True,exist_ok=True);bins=evidence/'bin';bins.mkdir(exist_ok=True);results=[]
 env=dict(os.environ,CC=args.cc,CGO_ENABLED='1',LMMCP_TEST_LMMOCK=str(mock/'lmmock'),LMMCP_TEST_PROGRAM=str(mock/'build/mcp.lmcode'))
 def run(name,command,extra=None):
  log=evidence/(name+'.log')
  with log.open('w') as output:result=subprocess.run(command,cwd=ROOT,env=dict(env,**(extra or {})),stdout=output,stderr=subprocess.STDOUT)
  results.append({'gate':name,'exit':result.returncode});(evidence/'results.json').write_text(json.dumps(results,indent=2)+'\n');print(json.dumps(results[-1]),flush=True)
  if result.returncode:
   sys.stderr.write(log.read_text());raise SystemExit(result.returncode)
 if not args.skip_prepare:run('prepare-mock',['bash','scripts/prepare-mock.sh','v0.1.111',str(mock)])
 tracked=subprocess.run(['git','ls-files'],cwd=ROOT,capture_output=True,text=True,check=True).stdout.splitlines()
 if any('__pycache__/' in p or p.endswith(('.pyc','.pyo','.pyd')) for p in tracked):raise SystemExit('tracked bytecode or __pycache__ found')
 run('notices',['python3','scripts/third_party_notices.py','--go',args.go,'--verify'])
 run('vet',[args.go,'vet','./...']);run('race',[args.go,'test','-race','-tags','integration','./...'])
 binaries=[]
 for system,arch in TARGETS:
  file=bins/('lmmcp' if (system,arch)==('linux','amd64') else 'lmmcp-'+system+'-'+arch)
  run('build-'+system+'-'+arch,[args.go,'build','-trimpath','-o',str(file),'./cmd/lmmcp'],{'CGO_ENABLED':'0','GOOS':system,'GOARCH':arch});binaries.append(file)
 (evidence/'builds.json').write_text(json.dumps([{'file':f.name,'sha256':hashlib.sha256(f.read_bytes()).hexdigest(),'bytes':f.stat().st_size} for f in binaries],indent=2)+'\n')
 run('binary-license-coverage',['python3','scripts/third_party_notices.py','--go',args.go,'--verify',*[x for f in binaries for x in ['--binary',str(f)]]])
 run('mock-build',[args.go,'test','-c','./internal/bridge','-o',str(bins/'mock-mcp')],{'CGO_ENABLED':'0'})
 run('e2e',['python3','scripts/e2e.py','--binary',str(bins/'lmmcp'),'--mock',str(bins/'mock-mcp'),'--evidence',str(evidence/'e2e')])
 for version in ['2026-07-28','2025-11-25','2024-11-05']:
  config=evidence/('mock-'+version+'.json');config.write_text(json.dumps({'protocolVersion':version,'bearerToken':''}))
  for mode in ['json','sse']:
   run('remote-'+version+'-'+mode,[str(mock/'lmmock'),'exec','--lmcode',str(mock/'build/mcp.lmcode'),'--config',str(config),'--','python3','scripts/remote-e2e.py','--binary',str(bins/'lmmcp'),'--lmscript',str(mock/'compiler/node_modules/.bin/lmscript'),'--version',version,'--mode',mode])
 for name in ([] if args.skip_mutations else ['mutations','mutations-slice2','mutations-wp358']):
  run(name,['python3','scripts/'+name+'.py','--go',args.go,'--evidence',str(evidence/name)],{'CGO_ENABLED':'0'})
 print(json.dumps({'ok':True,'gates':len(results),'mutationSuites':'separate-run' if args.skip_mutations else 'included','evidence':str(evidence)}))
 return 0
if __name__=='__main__':raise SystemExit(main())
