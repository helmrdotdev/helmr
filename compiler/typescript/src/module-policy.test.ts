import { expect, test } from "bun:test"
import { build } from "esbuild"
import { spawnSync } from "node:child_process"
import { mkdir, mkdtemp, rm, symlink, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { dirname, resolve } from "node:path"
import { pathToFileURL } from "node:url"

test("native provenance policy reads package imports from chunks without depending on cwd and rejects escaped code",async()=>{
 const temporary=await mkdtemp(resolve(tmpdir(),"helmr-native-policy-"))
 const root=resolve(temporary,"program"), policy=resolve(temporary,"policy.mjs"), cwd=resolve(temporary,"computer")
 try {
  await mkdir(cwd)
  for(const [name,body] of Object.entries({
   "package.json":JSON.stringify({type:"module",imports:{"#project/*":"./*"}}),
   "prompts/system.md":"prompt",
   "helmr/app/chunks/shared.mjs":`import {readFileSync} from 'node:fs'; export const value=readFileSync(new URL(import.meta.resolve('#project/prompts/system.md')),'utf8')`,
   "helmr/app/entry-0.mjs":`export {value} from './chunks/shared.mjs'`,
   "node_modules/pkg/package.json":JSON.stringify({name:"pkg",main:"index.cjs"}),
   "node_modules/pkg/index.cjs":`module.exports = 42`,
   "helmr/app/entry-1.mjs":`import{createRequire}from'node:module';export const value=createRequire(import.meta.url)('pkg')`,
   "bad.ts":"export const value:number=1",
  })) {const namePath=resolve(root,name);await mkdir(dirname(namePath),{recursive:true});await writeFile(namePath,body)}
  await writeFile(resolve(temporary,"outside.mjs"),"export const value=1")
  await symlink("../outside.mjs",resolve(root,"escaped.mjs"))
  await build({entryPoints:[new URL("../../../runtime/typescript/src/module-policy.ts",import.meta.url).pathname],bundle:true,platform:"node",format:"esm",outfile:policy})
  const mountedRoot = resolve(temporary, "mounted-program")
  await symlink(root, mountedRoot)
  const execute=(expression:string)=>spawnSync("node",["--no-strip-types","--no-global-search-paths","--input-type=module","--eval",`import {installModulePolicy} from ${JSON.stringify(pathToFileURL(policy).href)};installModulePolicy({root:${JSON.stringify(mountedRoot)}});${expression}`],{cwd,encoding:"utf8",env:{PATH:process.env["PATH"]}})
  for(const [name,value] of [["helmr/app/entry-0.mjs","prompt"],["helmr/app/entry-1.mjs","42"]]) {
   const result=execute(`console.log((await import(${JSON.stringify(pathToFileURL(resolve(mountedRoot,name!)).href)})).value)`)
   if (result.status !== 0) throw new Error(result.stderr);expect(result.status).toBe(0);expect(result.stdout.trim()).toBe(value!)
  }
  for(const name of ["bad.ts","escaped.mjs"]){
   const result=execute(`await import(${JSON.stringify(pathToFileURL(resolve(mountedRoot,name)).href)})`)
   expect(result.status).not.toBe(0);expect(result.stderr).toMatch(/generated JavaScript|inside Program/)
  }
 }finally{await rm(temporary,{recursive:true,force:true})}
})
