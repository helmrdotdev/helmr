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
  try {expect(runHostConfig(f.root)).toEqual({discovery:{dirs:["tasks"],ignorePatterns:[]},build:{builder:{steps:[]},secrets:[]}});expect(await readFile(resolve(f.root,"evaluated"),"utf8")).toBe("once")}finally{await f.close()}
 }
 for(const body of ["export const other={}","export default undefined","export default 1","export default []"]){
  const f=await fixture({"helmr.config.ts":body})
  try{expect(()=>runHostConfig(f.root)).toThrow("default-export a valid config object")}finally{await f.close()}
 }
 // The loader itself rejects a null module value before Helmr can inspect it.
 const f=await fixture({"helmr.config.ts":"export default null"})
 try{expect(()=>runHostConfig(f.root)).toThrow("failed to evaluate helmr.config.ts")}finally{await f.close()}
})

test("real packed SDK config/task/actor identity through a mixed JS-to-TS package",async()=>{
 const f=await fixture({
  "helmr.config.ts":`import{defineConfig}from'@helmr/sdk';import{dirs}from'./shared';export default defineConfig({dirs})`,
  "shared.ts":'export const dirs=["tasks"]',
  "node_modules/mixed/package.json":{type:"module",exports:{import:"./index.js",require:"./cjs.cts"}},
  "node_modules/mixed/index.js":'export{value}from"./value.ts"',
  "node_modules/mixed/value.ts":'import{readFileSync}from"node:fs";export const value:string=readFileSync(new URL("./asset",import.meta.url),"utf8")',
  "node_modules/mixed/asset":"installed",
  "node_modules/mixed/cjs.cts":'module.exports="required"',
  "tasks/task.ts":`import{task,actor}from'@helmr/sdk';import{value}from'mixed';import{createRequire}from'node:module';if(value!=='installed'||createRequire(import.meta.url)('mixed')!=='required')throw Error('wrong instance');export const build=task({id:'build',run:()=>value});export const worker=actor({id:'worker',run:async()=>{}})`,
 },true)
 try{
  const config=runHostConfig(f.root).discovery
  const result=await analyzeProject({root:f.root,architecture:"x86_64",config})
  expect(result.programDeclarations.map((d:{kind:string})=>d.kind)).toEqual(["task","actor"])
  expect(result.declarationLocator.declarations.map((d:{sourcePath:string})=>d.sourcePath)).toEqual(["tasks/task.ts","tasks/task.ts"])
  expect(Object.keys(result.result).sort()).toEqual(["apiVersion","config","discoveryCandidates","inputTreeDigest","language","nodeVersion","selections"])
 }finally{await f.close()}
})

test("program cannot replay root config through helper, and preserves missing imports",async()=>{
 for(const source of ['import "../helmr.config.ts?again"','import "missing-dependency"']){
  const f=await fixture({"helmr.config.ts":"throw Error('replayed')","tasks/main.ts":source})
  try{await expect(analyzeProject({root:f.root,architecture:"x86_64",config:{dirs:["tasks"],ignorePatterns:[]}})).rejects.toThrow(source.includes("again")?"build-only":"missing-dependency")}finally{await f.close()}
 }
})
