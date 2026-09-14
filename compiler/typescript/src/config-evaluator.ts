import { canonicalizeJsonValue, type JsonValue } from "@helmr/sdk/internal"
import { createWriteStream } from "node:fs"
import { installModuleExecution } from "@helmr/module-execution"
import { resolve } from "node:path"
import { loadConfig } from "./config"

const maxConfigBytes = 1 << 20

async function main(): Promise<void> {
  if (process.argv.length !== 4 || process.argv[2] === undefined || process.argv[3] === undefined) {
    throw new Error("Config Evaluator requires a Program root and exact Node version")
  }
  if (process.versions.node !== process.argv[3]) throw new Error("Config Evaluator Node version does not match Runtime")
  const execution = installModuleExecution({ root: process.argv[2], phase: "config" })
  const config = await loadConfig(resolve(execution.root, "helmr.config.ts"), execution.importSourceExports)
  const body = canonicalizeJsonValue(config as unknown as JsonValue)
  if (body.byteLength === 0 || body.byteLength > maxConfigBytes) {
    throw new Error("normalized config size is invalid")
  }
  const frame = new Uint8Array(4 + body.byteLength)
  new DataView(frame.buffer).setUint32(0, body.byteLength, false)
  frame.set(body, 4)
  const configured = process.env["HELMR_SUPERVISOR_FD"]
  const fd = configured === undefined ? 3 : Number(configured)
  if (!Number.isSafeInteger(fd) || fd < 3) {
    throw new Error("Config Evaluator result descriptor is invalid")
  }
  const output = createWriteStream("", { fd, autoClose: false })
  await new Promise<void>((resolve, reject) => {
    output.once("error", reject)
    output.end(frame, resolve)
  })
}

await main()
