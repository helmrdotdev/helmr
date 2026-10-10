import { spawnSync } from "node:child_process"
import { test, expect } from "bun:test"
import { cp, mkdir, mkdtemp, readFile, realpath, rm, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { dirname, resolve } from "node:path"
import { analyzeProject, runHostConfig } from "./test-process"

const repository = new URL("../../../", import.meta.url).pathname
async function fixture(files: Record<string, string | object>, sdk = false) {
 const root=await realpath(await mkdtemp(resolve(tmpdir(),"helmr-native-compiler-")))
 for(const [name,value] of Object.entries({"package.json":{type:"module"},...files})) {
  const target=resolve(root,name);await mkdir(dirname(target),{recursive:true});await writeFile(target,typeof value==="string"?value:JSON.stringify(value))
 }
 if(sdk){
  for(const name of ["sdk","proto"]) await cp(resolve(repository,`dist/npm/${name}/package`),resolve(root,`node_modules/@helmr/${name}`),{recursive:true})
  await cp(await realpath(resolve(repository,"sdk/typescript/node_modules/@bufbuild/protobuf")),resolve(root,"node_modules/@bufbuild/protobuf"),{recursive:true,dereference:true})
 }
 return {root,close:()=>rm(root,{recursive:true,force:true})}
}

test("config defaults are strict, computed ESM/CJS expressions execute once", async()=>{
 for(const type of ["module","commonjs"]){
  const f=await fixture({"package.json":{type},"helmr.config.ts":`import{writeFileSync}from'node:fs';writeFileSync(new URL('./evaluated',import.meta.url),'once',{flag:'wx'});const value={};export default value`})
  // CJS uses its native filename, not import.meta (which is ESM-only).
  if(type==="commonjs")await writeFile(resolve(f.root,"helmr.config.ts"),`import{writeFileSync}from'node:fs';writeFileSync(__dirname+'/evaluated','once',{flag:'wx'});const value={};export default value`)
  try {expect(runHostConfig(f.root)).toEqual({discovery:{assets:[],dirs:["tasks"],external:[],ignorePatterns:[]},build:{builder:{steps:[]},secrets:[]}});expect(await readFile(resolve(f.root,"evaluated"),"utf8")).toBe("once")}finally{await f.close()}
 }
 for(const body of ["export const other={}","export default undefined","export default 1","export default []"]){
  const f=await fixture({"helmr.config.ts":body})
  try{expect(()=>runHostConfig(f.root)).toThrow("default-export a valid config object")}finally{await f.close()}
 }
 // The loader itself rejects a null module value before Helmr can inspect it.
 const f=await fixture({"helmr.config.ts":"export default null"})
 try{expect(()=>runHostConfig(f.root)).toThrow("failed to evaluate helmr.config.ts")}finally{await f.close()}
})

test("packed SDK identity survives one split bundle and assets resolve independently of cwd",async()=>{
 const f=await fixture({
  "package.json":{type:"module",imports:{"#project/*":"./*"}},
  "helmr.config.ts":`import{defineConfig}from'@helmr/sdk';export default defineConfig({build:{assets:["prompts/**"]}})`,
  "prompts/system.md":"installed",
  "shared.ts":`import{agent,computer,image}from'@helmr/sdk';import{readFileSync}from'node:fs';export const value=readFileSync(new URL(import.meta.resolve('#project/prompts/system.md')),'utf8');export const shared=agent({id:'build',computer:computer({id:'machine',image:image('root').from('ubuntu:24.04'),resources:{cpu:1,memory:'1GiB'}}),turn:()=>value})`,
  "tasks/a.ts":`export{shared as build}from'../shared'`,
  "tasks/b.ts":`import{agent}from'@helmr/sdk';import{value,shared}from'../shared';if(value!=='installed')throw Error('wrong asset');export const worker=agent({id:'worker',computer:shared.computer,turn:async()=>null})`,
 },true)
 try{
  const result=await analyzeProject({root:f.root,architecture:"x86_64",config:runHostConfig(f.root).discovery})
  expect(result.buildPlan.definitions.map((d:{kind:string})=>d.kind)).toEqual(["agent","agent","computer"])
  expect(result.definitionIndex.agents.map((d:{modulePath:string})=>d.modulePath)).toEqual(["helmr/app/entry-0.mjs","helmr/app/entry-1.mjs"])
  expect(Object.keys(result.result).sort()).toEqual(["apiVersion","bundler","config","modules","nodeVersion","payloadDigest"])
 }finally{await f.close()}
})

test("program cannot replay root config through helper, and preserves missing imports",async()=>{
 for(const source of ['import "../helmr.config.ts?again"','import "missing-dependency"']){
  const f=await fixture({"helmr.config.ts":"throw Error('replayed')","tasks/main.ts":source})
  try{await expect(analyzeProject({root:f.root,architecture:"x86_64",config:{dirs:["tasks"],ignorePatterns:[]}})).rejects.toThrow(source.includes("again")?"build-only":"missing-dependency")}finally{await f.close()}
 }
})


test("packed SDK exposes coherent Agent handler and preparation types", async () => {
 const f = await fixture({ "authoring.ts": `
   import { agent, computer, image, triggers, type CronTrigger, type Turn, type AgentContext, type AgentDefinition, type BuildContext, type ComputerDefinition, type InputContent } from '@helmr/sdk';
   const prepare = async (build: BuildContext): Promise<void> => { await build.exec(['true']); };
   const machine: ComputerDefinition = computer({ id: 'computer', image: image('root').from('ubuntu:24.04'), resources: { cpu: 1, memory: '1GiB' }, prepare });
   const handler = async (turn: Turn<InputContent>, ctx: AgentContext<{ value: number }>): Promise<number> => {
     const { answer } = await turn.ask({ prompt: [{ type: 'text', text: 'continue' }], answer: { type: 'text' } });
     await turn.output.write([{ type: 'json', value: answer }]);
     if (ctx.session.parent) ctx.session.parent.id satisfies string;
     return ctx.setupResult.value;
   };
   const nightly: CronTrigger<InputContent> = triggers.cron('nightly', '0 0 * * *', { timezone: 'Asia/Tokyo', input: [] });
   const worker: AgentDefinition<InputContent, number, { value: number }> = agent({ id: 'agent', computer: machine, setup: () => ({ value: 42 }), turn: handler, triggers: [nightly] });
   void worker;
 ` }, true)
 try {
   const compiler = resolve(repository, 'node_modules/typescript/bin/tsc')
   const result = spawnSync('node', [compiler, '--noEmit', '--strict', '--skipLibCheck', '--target', 'ESNext', '--module', 'NodeNext', '--moduleResolution', 'NodeNext', '--lib', 'ESNext,DOM', resolve(f.root, 'authoring.ts')], { cwd: f.root, encoding: 'utf8' })
   expect({ status: result.status, output: result.stdout + result.stderr }).toEqual({ status: 0, output: '' })
 } finally { await f.close() }
})
