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
} from "./bundle"
import { inspectCanonicalConfig } from "./config"

async function main(): Promise<void> {
  if (process.argv.length === 3 && process.argv[2] === "--describe") {
    process.stdout.write(canonicalizeJsonValue(compilerContract()))
    return
  }
  if (
    process.argv.length !== 6 ||
    process.argv[2] === undefined ||
    process.argv[3] === undefined ||
    process.argv[4] === undefined ||
    process.argv[5] === undefined
  ) {
    throw new Error(
      "Program Compiler requires a Program root, canonical config path, exact Node version, and output root",
    )
  }
  const root = resolve(process.argv[2])
  const config = inspectCanonicalConfig(
    JSON.parse(await readFile(process.argv[3], "utf8")),
  )
  const compiled = await compileProgram({
    architecture: "x86_64",
    config,
    nodeVersion: process.argv[4],
    outputRoot: process.argv[5],
    root,
  })
  for (const [path, contents] of compiled.files) {
    const target = resolve(process.argv[5], path)
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
  if (process.argv[2] === "--describe") throw error
  const message = error instanceof Error ? error.message : String(error)
  await writeResult(failedVerificationResult(message))
}
