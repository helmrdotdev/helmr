import {spawnSync} from 'node:child_process';
import {performance} from 'node:perf_hooks';
import {readFileSync,writeFileSync,appendFileSync,statSync} from 'node:fs';
import {createHash} from 'node:crypto';
const entry=process.argv[2],label=process.argv[3],output='/out/'+label;
if (process.version !== 'v24.21.0') throw Error('benchmark requires exact Node24.21.0');
const hash=p=>createHash('sha256').update(readFileSync(p)).digest('hex');
let order=0;
writeFileSync(output+'.environment.json',JSON.stringify({node:process.version,arch:process.arch,platform:process.platform,nodeSHA256:hash(process.execPath),toolchain:JSON.parse(readFileSync('/bench/identity.json','utf8'))}));
function run(mode,extra={}){
 const args=['--no-strip-types','--no-global-search-paths','--enable-source-maps','/bench/runner.mjs',JSON.stringify({mode,entry:['bare','api','entry'].includes(mode)?undefined:entry,...extra})];
 const t=performance.now();const child=spawnSync(process.execPath,args,{encoding:'utf8',env:{PATH:process.env.PATH},maxBuffer:16*1024*1024});const elapsedMs=performance.now()-t;
 const row={order:order++,label,mode,argv:[process.execPath,...args],elapsedMs,status:child.status,stderr:child.stderr,stdout:child.stdout};
 appendFileSync(output+'.jsonl',JSON.stringify(row)+'\n');
 if(child.status!==0)throw Error(JSON.stringify(row));
 return JSON.parse(child.stdout.trim().split('\n').at(-1));
}
const cachePath=output+'.cache.json';
// Capture during the ordinary graph import used to validate its result, not by
// scanning dormant files or calling handlers to warm a cache.
const reference=run('record',{cachePath});const cacheDigest=hash(cachePath);
writeFileSync(output+'.cache-receipt.json',JSON.stringify({cachePath,cacheDigest,sizeBytes:statSync(cachePath).size,reference},null,2));
const modes=['plain','current','reuse'];
for(let trial=0;trial<9;trial++){
 for(let offset=0;offset<3;offset++){
  const mode=modes[(trial+offset)%3];const result=run(mode,{cachePath,cacheDigest});
  if(JSON.stringify(result.value)!==JSON.stringify(reference.value))throw Error('mode changed graph value');
 }
}
for(const mode of ['bare','api','entry'])for(let i=0;i<5;i++)run(mode);
if(label==='tiny-ts')for(const mode of ['plain','current','reuse'])run(mode,{cachePath,cacheDigest,query:true});
console.log(JSON.stringify({label,status:'pass',processes:order,cacheBytes:statSync(cachePath).size,cacheDigest}));
