from pathlib import Path
import hashlib,json,shutil,sys,os
repo=Path(sys.argv[1]);out=Path(sys.argv[2]);source=Path(__file__).parent
platform=out/'platform';platform.mkdir(parents=True)
def sha(path):return hashlib.sha256(path.read_bytes()).hexdigest()
for name,target in [('loader.mjs','native.mjs'),('typescript.cjs','typescript.cjs')]:shutil.copy2(repo/'internal/moduleexecution'/name,platform/target)
# A temporary instrumented copy of the exact generated production bytes.
s=(platform/'native.mjs').read_text();replacements={
 'ts2.transpileModule(readFileSync2(canonical, "utf8"), {':'emitProbe(ts2.transpileModule, readSource(canonical), {',
 'authority.config(parent)':'readConfig(() => authority.config(parent))',
 'authority.config(canonical)':'readConfig(() => authority.config(canonical))'}
for old,new in replacements.items():
 if s.count(old)!=1:raise RuntimeError('instrumentation point changed: '+old)
 s=s.replace(old,new)
(platform/'instrumented.mjs').write_text('import {emitProbe,readSource,readConfig} from "./probe.mjs";\n'+s)
shutil.copy2(source/'probe.mjs',platform/'probe.mjs')
for name in ['runner.mjs','controller.mjs']:shutil.copy2(source/name,out/name)
(out/'identity.json').write_text(json.dumps({name:sha(platform/name) for name in ['native.mjs','typescript.cjs']},sort_keys=True)+'\n')
def put(root,name,value):
 path=root/name;path.parent.mkdir(parents=True,exist_ok=True);path.write_text(value if isinstance(value,str) else json.dumps(value))
for name,typed in [('tiny-js',False),('tiny-ts',True)]:
 root=out/'workloads'/name;put(root,'package.json',{'type':'module'})
 put(root,'main.'+('ts' if typed else 'js'),'globalThis.__benchmarkEvaluations=(globalThis.__benchmarkEvaluations??0)+1;export const value'+(': number' if typed else '')+'=42;')
 put(root,'dormant.ts','export const broken: = ;')
root=out/'workloads/ts-heavy';put(root,'package.json',{'type':'module'});put(root,'node_modules/local/package.json',{'name':'local','type':'module','exports':'./index.ts'})
put(root,'node_modules/local/tsconfig.json',{'compilerOptions':{'target':'es2022','useDefineForClassFields':True,'paths':{'@helper':['./helper.ts']}}})
put(root,'node_modules/local/helper.ts','export const offset: number=1;');exports=[]
for i in range(160):
 put(root,f'node_modules/local/part{i}.ts',f'import {{offset}} from "@helper";\n'+''.join(f'export class C{j} {{value:number={i+j};read():number{{return this.value+offset}}}}\n' for j in range(12))+f'export const value{i}:number=new C0().read();')
 exports.append(f'import{{value{i}}}from"./part{i}";')
put(root,'node_modules/local/index.ts','\n'.join(exports)+'\nexport const value:number='+ '+'.join(f'value{i}' for i in range(160))+';')
put(root,'node_modules/local/dormant.ts','export const invalid: = ;');put(root,'main.ts','export{value}from"local";')
root=out/'workloads/public-sdk';put(root,'package.json',{'type':'module'})
shutil.copytree(out/'public-install/node_modules',root/'node_modules',symlinks=True)
for name in ['sdk','proto']:shutil.copytree(repo/f'dist/npm/{name}/package',root/f'node_modules/@helmr/{name}')
shutil.copytree((repo/'sdk/typescript/node_modules/@bufbuild/protobuf').resolve(),root/'node_modules/@bufbuild/protobuf')
put(root,'main.ts','import {task,actor} from "@helmr/sdk";import{createOpencodeClient}from"@opencode-ai/sdk/v2";export const value=[typeof task,typeof actor,typeof createOpencodeClient];')
workloads={'tiny-js':'main.js','tiny-ts':'main.ts','ts-heavy':'main.ts','public-sdk':'main.ts'}
(out/'workloads.json').write_text(json.dumps(workloads,indent=2)+'\n')
manifest=[]
for name,entry in workloads.items():
 root=out/'workloads'/name;files=[]
 for directory,dirs,names in os.walk(root,followlinks=False):
  for leaf in sorted(dirs+names):
   path=Path(directory)/leaf;record=dict(path=str(path.relative_to(root)),mode=oct(path.lstat().st_mode&0o777))
   if path.is_symlink():record['link']=os.readlink(path)
   elif path.is_file():record.update(size=path.stat().st_size,sha256=sha(path))
   else:continue
   files.append(record)
 inventory=json.dumps(sorted(files,key=lambda r:r['path']),sort_keys=True,separators=(',',':')).encode();(out/f'{name}.input-files.json').write_bytes(inventory)
 manifest.append(dict(name=name,entryURL='file:///project/'+entry,inventorySHA256=hashlib.sha256(inventory).hexdigest(),sourceBytes=sum(f.get('size',0) for f in files),files=len(files)))
(out/'workload-manifest.json').write_text(json.dumps(manifest,indent=2)+'\n')
