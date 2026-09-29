import { canonicalizeJsonValue } from "@helmr/sdk/internal"
import { createWriteStream } from "node:fs"
import { mkdir, readFile, writeFile } from "node:fs/promises"
import { dirname, resolve } from "node:path"

import {
  encodeVerificationResultFrame,
  failedVerificationResult,
  successfulVerificationResult,
  type VerificationResultFrame,
} from "./analysis"
import {
  compileProgram,
  compilerContract,
} from "./source"
import { bundleProgram } from "./bundle"
import { assembleRuntime, installRuntimePackages } from "./runtime-packages"
import { inspectCanonicalConfig } from "./config"

async function main(): Promise<void> {
  if (process.argv.length === 3 && process.argv[2] === "--describe") {
    process.stdout.write(canonicalizeJsonValue(compilerContract()))
    return
  }
  const [mode, ...args] = process.argv.slice(2)
  if (mode === "--bundle" && args.length === 4) {
    if (args[2] !== process.versions.node) throw new Error("bundler Node version does not match the Runtime")
    await bundleProgram({ root: args[0]!, config: inspectCanonicalConfig(JSON.parse(await readFile(args[1]!, "utf8"))), nodeVersion: args[2]!, output: args[3]! })
    return
  }
  if (mode === "--install-runtime" && args.length === 0) { await installRuntimePackages(); return }
  if (mode === "--assemble" && args.length === 3) {
    await assembleRuntime({ bundle: args[0]!, installed: args[1]!, output: args[2]! })
    return
  }
  if (mode !== "--analyze" || args.length !== 6) throw new Error("Program Compiler requires --bundle, --assemble or --analyze with complete inputs")
  const config = inspectCanonicalConfig(JSON.parse(await readFile(args[1]!, "utf8")))
  const compiled = await compileProgram({
    architecture: "x86_64", root: resolve(args[0]!), config,
    nodeVersion: args[2]!, payloadDigest: args[3]!, bundlePath: args[4]!,
  })
  for (const [path, contents] of compiled.files) {
    const target = resolve(args[5]!, path)
    await mkdir(dirname(target), { recursive: true })
    await writeFile(target, contents)
  }
  await writeResult(successfulVerificationResult(compiled.analysis))
}

async function writeResult(result: VerificationResultFrame): Promise<void> {
  const configured = process.env["HELMR_SUPERVISOR_FD"]
  const fd = configured === undefined ? 3 : Number(configured)
  if (!Number.isSafeInteger(fd) || fd < 3) {
    throw new Error("Program Compiler result descriptor is invalid")
  }
  const output = createWriteStream("", { fd, autoClose: false })
  const frame = encodeVerificationResultFrame(result)
  await new Promise<void>((resolve, reject) => {
    output.once("error", reject)
    output.end(frame, resolve)
  })
}

try {
  await main()
} catch (error) {
  if (process.argv[2] !== "--analyze") throw error
  const message = error instanceof Error ? error.message : String(error)
  await writeResult(failedVerificationResult(message))
}
