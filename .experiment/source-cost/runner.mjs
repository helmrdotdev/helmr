import { performance } from 'node:perf_hooks';
import { resourceUsage, memoryUsage } from 'node:process';
import { createHash } from 'node:crypto';
import { readFileSync } from 'node:fs';
if (process.version !== 'v24.21.0') throw Error('benchmark requires exact Node24.21.0');
const options=JSON.parse(process.argv[2]), start=performance.now();
const phase={apiImportMs:0,adapterImportMs:0,entryGuardMs:0,graphMs:0};
let adapter,probe,execution;
const identity=createHash('sha256').update(readFileSync('/bench/identity.json')).digest('hex');
if(options.mode==='api'||options.mode!=='plain'&&options.mode!=='bare'){
 const t=performance.now();await import('/platform/typescript.cjs');phase.apiImportMs=performance.now()-t;
}
if(!['bare','api'].includes(options.mode)){
 const t=performance.now();adapter=await import(options.mode==='plain'?'/platform/native.mjs':'/platform/instrumented.mjs');phase.adapterImportMs=performance.now()-t;
 if(options.mode!=='plain'){probe=await import('/platform/probe.mjs');probe.configure({mode:options.mode,identity,cachePath:options.cachePath,cacheDigest:options.cacheDigest});}
 const guard=performance.now();execution=adapter.installModuleExecution({root:'/project',phase:'program'});phase.entryGuardMs=performance.now()-guard;
}
let value, warmMs=[], queryMs, queryDistinct;
if(options.entry){
 const url=new URL(options.entry,'file:///project/'),t=performance.now();
 const namespace=await import(url.href);value=namespace.value;phase.graphMs=performance.now()-t;
 for(let i=0;i<5;i++){const t=performance.now();if(await import(url.href)!==namespace)throw Error('native module cache identity changed');warmMs.push(performance.now()-t);}
 if(options.query){const before=performance.now();const other=await import(url.href+'?probe=1#identity');queryMs=performance.now()-before;queryDistinct=other!==namespace;if(!queryDistinct||JSON.stringify(other.value)!==JSON.stringify(value))throw Error('query identity/value changed');}
}
const executionMs=performance.now()-start;
probe?.finish();
const metrics=probe?{...probe.metrics}:undefined;
console.log(JSON.stringify({mode:options.mode,entry:options.entry,value,node:process.version,arch:process.arch,platform:process.platform,phase,executionMs,warmMs,queryMs,queryDistinct,evaluationCount:globalThis.__benchmarkEvaluations,metrics,resourceUsage:resourceUsage(),memory:memoryUsage(),configReads:execution?[...execution.configReads]:[]}));
