import { expect, test } from "bun:test"
import { mkdir, mkdtemp, readFile, readdir, rm, symlink, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { dirname, resolve } from "node:path"
import { bundleProgram } from "./bundle"
import { spawnSync } from "node:child_process"
import { fileURLToPath } from "node:url"

async function fixture(files: Record<string, unknown>) {
 const temporary = await mkdtemp(resolve(tmpdir(), "helmr-bundle-"))
 const root = resolve(temporary,"project"), output = resolve(temporary,"bundle")
 for (const [name, value] of Object.entries({"package.json":{type:"module"},"tasks/main.ts":"export const value: number = 1",...files})) {
  const target = resolve(root,name); await mkdir(dirname(target),{recursive:true}); await writeFile(target,typeof value === "string" ? value : JSON.stringify(value))
 }
 return {root,output,close:()=>rm(temporary,{recursive:true,force:true})}
}
const config = {dirs:["tasks"],ignorePatterns:[],external:[],assets:[]}
const run = (f: {root:string;output:string}, overrides = {}) => bundleProgram({...f,nodeVersion:"24.21.0",config:{...config,...overrides}})

test("selective payload preserves root imports and explicit assets, excluding author source and dev dependencies",async()=>{
 const f=await fixture({"package.json":{name:"fixture",type:"module",imports:{"#project/*":"./*"},scripts:{prepare:"exit 1"},devDependencies:{unused:"1"}},"prompts/a.md":"prompt","node_modules/unused/index.js":"unused"})
 try {
  await run(f,{assets:["prompts/**"]})
  expect((await readdir(resolve(f.output,"payload"))).sort()).toEqual(["helmr","package.json","prompts"])
  expect(JSON.parse(await readFile(resolve(f.output,"payload/package.json"),"utf8"))).toEqual({name:"fixture",type:"module",imports:{"#project/*":"./*"},private:true,dependencies:{}})
  expect(await readFile(resolve(f.output,"payload/prompts/a.md"),"utf8")).toBe("prompt")
 }finally{await f.close()}
})

test("external roots use installed exact versions even without static imports and retain npm overrides only for installation",async()=>{
 const f=await fixture({"package.json":{dependencies:{tool:"^1.0.0"},overrides:{transitive:"2.0.0"}},"node_modules/tool/package.json":{name:"tool",version:"1.2.3"}})
 try{
  await run(f,{external:["tool"]})
  expect(JSON.parse(await readFile(resolve(f.output,"install/package.json"),"utf8"))).toEqual({private:true,dependencies:{tool:"1.2.3"},overrides:{transitive:"2.0.0"}})
  expect(JSON.parse(await readFile(resolve(f.output,"payload/package.json"),"utf8")).overrides).toBeUndefined()
 }finally{await f.close()}
})

test("unsupported root selectors, missing installs and dependency override dialects fail before runtime installation",async()=>{
 for(const files of [
  {"package.json":{dependencies:{tool:"workspace:*"}}},
  {"package.json":{dependencies:{tool:"npm:other@1"}}},
  {"package.json":{dependencies:{tool:"^1"}}},
  {"package.json":{resolutions:{tool:"1"}}},
  {"pnpm-workspace.yaml":"overrides:\n  tool: 1\n"},
  {".pnpmfile.cjs":"module.exports={}"},
 ]){
  const f=await fixture(files)
  try{await expect(run(f,{external:["tool"]})).rejects.toThrow()}finally{await f.close()}
 }
})

test("assets reject missing matches, managed namespaces and symlink traversal",async()=>{
 for(const pattern of ["missing/**","package.json","node_modules/**","linked/**"]){
  const f=await fixture({"assets/file.txt":"body","node_modules/tool/index.js":"body"})
  try{await symlink("assets",resolve(f.root,"linked"));await expect(run(f,{assets:[pattern]})).rejects.toThrow()}finally{await f.close()}
 }
})

test("package tsconfig extends and path aliases are resolved during bundling",async()=>{
 const f=await fixture({"tsconfig.json":{extends:"@config/base/config.json",compilerOptions:{baseUrl:".",paths:{"@/*":["src/*"]}}},"node_modules/@config/base/package.json":{name:"@config/base",version:"1.0.0"},"node_modules/@config/base/config.json":{compilerOptions:{target:"ESNext",module:"Preserve"}},"src/value.ts":"export const value:number=42","tasks/main.ts":"export {value} from '@/value'"})
 try{await run(f);expect(await readFile(resolve(f.output,"payload/helmr/app/entry-0.mjs"),"utf8")).toContain("42")}finally{await f.close()}
})

test("runtime lock admission accepts bundled records but rejects unsupported transitive sources",async()=>{
 for(const [record,accepted] of [[{inBundle:true,version:"1.0.0"},true],[{resolved:"file:../local",integrity:"value"},false],[{resolved:"https://private.example/tool.tgz",integrity:"value"},false]] as const){
  const f=await fixture({})
  try{
   await run(f)
   const installed=resolve(f.output,"install")
   await writeFile(resolve(installed,"package-lock.json"),JSON.stringify({lockfileVersion:3,packages:{"":{},"node_modules/tool":record}}))
   await mkdir(resolve(installed, "node_modules"))
   await writeFile(resolve(installed, "node_modules/.package-lock.json"), "{}")
   const output = resolve(f.output, "assembled")
   const child = spawnSync("node", [
     fileURLToPath(new URL("../../../internal/compiler/program-compiler.mjs", import.meta.url)),
     "--assemble", f.output, installed, output,
   ], { encoding: "utf8" })
   if (accepted) {
     expect(child.status, child.stderr).toBe(0)
     expect(await readdir(resolve(output, "node_modules"))).not.toContain(".package-lock.json")
   } else {
     expect(child.status).not.toBe(0)
     expect(child.stderr).toContain("unsupported source")
   }
  }finally{await f.close()}
 }
})

test("ordinary createRequire bindings remain executable after bundling",async()=>{
 const f=await fixture({"tasks/main.ts":`import {createRequire} from 'node:module';const require=createRequire(import.meta.url);export const value=require('node:path').sep`})
 try{
  await run(f)
  const {spawnSync}=await import("node:child_process")
  const child=spawnSync("node",[resolve(f.output,"payload/helmr/app/entry-0.mjs")],{encoding:"utf8"})
  if(child.status!==0)throw new Error(child.stderr)
 }finally{await f.close()}
})

test("Program imports cannot reach a symlinked build config through its canonical target", async () => {
  for (const specifier of ["../helmr.config.ts", "../config/project.ts"]) {
    const f = await fixture({
      "config/project.ts": "export const setting = 42",
      "tasks/main.ts": `export { setting } from ${JSON.stringify(specifier)}`,
    })
    try {
      await symlink("config/project.ts", resolve(f.root, "helmr.config.ts"))
      await expect(run(f)).rejects.toThrow("helmr.config.ts is build-only")
    } finally {
      await f.close()
    }
  }
})

test("generated source maps report the original TypeScript location", async () => {
  const f = await fixture({
    "tasks/main.ts": "const message: string = 'source-map-probe'\nthrow new Error(message)\n",
  })
  try {
    await run(f)
    const { spawnSync } = await import("node:child_process")
    await rm(f.root, { recursive: true })
    const child = spawnSync("node", [
      "--enable-source-maps",
      resolve(f.output, "payload/helmr/app/entry-0.mjs"),
    ], { encoding: "utf8" })
    expect(child.status).not.toBe(0)
    expect(child.stderr).toContain("tasks/main.ts:2")
    const map = JSON.parse(await readFile(resolve(f.output, "payload/helmr/app/entry-0.mjs.map"), "utf8"))
    expect(map.sourcesContent).toContain("const message: string = 'source-map-probe'\nthrow new Error(message)\n")
  } finally {
    await f.close()
  }
})
