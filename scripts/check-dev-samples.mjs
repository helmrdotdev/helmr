import { nodeVersion } from "./node-version.mjs"
import { requireVersion } from "./check-node-toolchain.mjs"
import { spawnSync } from "node:child_process"
import { mkdtempSync, writeFileSync, readFileSync, openSync, closeSync, rmSync } from "node:fs"
import { tmpdir } from "node:os"
import { resolve } from "node:path"

requireVersion(process.versions.node, "sample compiler interpreter")
const output = mkdtempSync(resolve(tmpdir(),"helmr-sample-analysis-"))
try {
 for(const [name,dirs] of [["execution",["dev/workflows/tasks"]],["schedule",["dev/schedule-workflows/tasks"]]]) {
  const config=resolve(output,`${name}-config.json`)
  writeFileSync(config,JSON.stringify({dirs,ignorePatterns:[]}))
  const framePath=resolve(output,`${name}-result`)
  const fd=openSync(framePath,"w")
  let child
  try {
   // This sample check exercises analysis only. Go's preparation/admission tests
   // own the installed input digest; the compiler simply carries that authority.
   child=spawnSync(process.execPath,["--no-strip-types","--no-global-search-paths","--enable-source-maps","internal/compiler/program-compiler.mjs",process.cwd(),config,nodeVersion,`sha256:${"0".repeat(64)}`,resolve(output,name)],{stdio:["ignore","inherit","inherit",fd],env:{PATH:process.env.PATH}})
  }finally{closeSync(fd)}
  if(child.status!==0)throw new Error(`sample compiler exited ${child.status}`)
  const frame=readFileSync(framePath)
  if(frame.readUInt32BE(0)!==frame.length-4)throw new Error("invalid sample verification frame")
  const result=JSON.parse(frame.subarray(4))
  if(result.outcome!=="succeeded")throw new Error(result.error.message)
  const plan=JSON.parse(result.files[0].content)
  const tasks=plan.definitions.filter(d=>d.kind==="task")
  if(name==="execution"&&tasks.some(d=>d.manifest.schedule!==undefined))throw new Error("ordinary dev workflows must be schedule-free")
  if(name==="schedule"&&(tasks.length!==1||tasks[0].manifest.schedule===undefined||tasks[0].declaredId!=="schedule-smoke"||tasks[0].manifest.run.ttlMs!==300000))throw new Error("Schedule fixture must analyze one bounded schedule-smoke Task")
  console.log(`analyzed ${name}: ${plan.definitions.length} definitions`)
 }
}finally{rmSync(output,{recursive:true,force:true})}
