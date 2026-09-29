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
  "shared.ts":`import{task}from'@helmr/sdk';import{readFileSync}from'node:fs';export const value=readFileSync(new URL(import.meta.resolve('#project/prompts/system.md')),'utf8');export const shared=task({id:'build',run:()=>value})`,
  "tasks/a.ts":`export{shared as build}from'../shared'`,
  "tasks/b.ts":`import{actor}from'@helmr/sdk';import{value}from'../shared';if(value!=='installed')throw Error('wrong asset');export const worker=actor({id:'worker',run:async()=>{}})`,
 },true)
 try{
  const result=await analyzeProject({root:f.root,architecture:"x86_64",config:runHostConfig(f.root).discovery})
  expect(result.programDeclarations.map((d:{kind:string})=>d.kind)).toEqual(["task","actor"])
  expect(result.declarationLocator.declarations.map((d:{modulePath:string})=>d.modulePath)).toEqual(["helmr/app/entry-0.mjs","helmr/app/entry-1.mjs"])
  expect(Object.keys(result.result).sort()).toEqual(["apiVersion","bundler","config","modules","nodeVersion","payloadDigest","selections"])
 }finally{await f.close()}
})

test("program cannot replay root config through helper, and preserves missing imports",async()=>{
 for(const source of ['import "../helmr.config.ts?again"','import "missing-dependency"']){
  const f=await fixture({"helmr.config.ts":"throw Error('replayed')","tasks/main.ts":source})
  try{await expect(analyzeProject({root:f.root,architecture:"x86_64",config:{dirs:["tasks"],ignorePatterns:[]}})).rejects.toThrow(source.includes("again")?"build-only":"missing-dependency")}finally{await f.close()}
 }
})
