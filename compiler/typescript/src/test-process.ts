// Integration harness runs the generated, platform-owned entries under Node.
import { spawnSync } from "node:child_process"
import { readFileSync,mkdtempSync,rmSync } from "node:fs"
import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { resolve } from "node:path"
import type { HelmrConfig } from "@helmr/sdk/internal"

export const nodeVersion = spawnSync("node", ["-p", "process.versions.node"], { encoding: "utf8" }).stdout.trim()
export function runEntry(name: string, args: string[]) {
  const entry = new URL(`../../../internal/compiler/${name}.mjs`, import.meta.url)
  const scratch=mkdtempSync(resolve(tmpdir(),"helmr-frame-"))
  const resultPath=resolve(scratch,"frame")
  let frame:Buffer
  try {
    const child = spawnSync("bash", ["-c", 'exec "$@" 3>"$HELMR_TEST_RESULT"', "helmr-test", "node", "--no-strip-types", "--no-global-search-paths", "--enable-source-maps", entry.pathname, ...args], {
      stdio: ["ignore", "pipe", "pipe"], env: { PATH: process.env["PATH"], HELMR_TEST_RESULT:resultPath },
    })
    frame=readFileSync(resultPath)
    if (child.error !== undefined || child.status !== 0 || frame.length < 4) throw new Error(`${child.error ?? "compiler failed"} status=${child.status}: ${child.stderr?.toString()}`)
  } finally {rmSync(scratch,{recursive:true,force:true})}
  const body = Buffer.from(frame)
  if (body.readUInt32BE(0) !== body.length - 4) throw new Error("invalid compiler frame")
  return JSON.parse(body.subarray(4).toString())
}
export async function analyzeProject(options: {root: string; architecture: "x86_64"; config: HelmrConfig}) {
  const output = await mkdtemp(resolve(tmpdir(), "helmr-source-result-"))
  try {
    const configPath = resolve(output,"config.json")
    await writeFile(configPath, JSON.stringify(options.config))
    const verification = runEntry("program-compiler", [options.root,configPath,nodeVersion,`sha256:${"1".repeat(64)}`,output])
    if (verification.outcome !== "succeeded") throw new Error(verification.error.message)
    const result = JSON.parse(await readFile(resolve(output,"helmr/compiler-result.json"),"utf8"))
    return {buildPlan: JSON.parse(verification.files[0].content),declarationLocator:JSON.parse(verification.files[1]?.content ?? '{"declarations":[]}'),programDeclarations:verification.declarations,modules:result.discoveryCandidates,result}
  } finally {await rm(output,{recursive:true,force:true})}
}
