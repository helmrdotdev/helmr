import { canonicalizeJsonValue, type JsonValue } from "@helmr/sdk/internal"
import { createWriteStream } from "node:fs"
import { compileConfig } from "./bundle"
import { loadConfig } from "./config"

const maxConfigBytes = 1 << 20

async function main(): Promise<void> {
  if (
    process.argv.length !== 6 ||
    process.argv[2] === undefined ||
    process.argv[3] === undefined ||
    process.argv[4] === undefined ||
    !isManager(process.argv[5])
  ) {
    throw new Error(
      "Config Evaluator requires a Program root, exact Node version, output root, and Manager family",
    )
  }
  const compiled = await compileConfig({
    manager: process.argv[5],
    nodeVersion: process.argv[3],
    outputRoot: process.argv[4],
    root: process.argv[2],
  })
  let config
  try {
    config = await loadConfig(compiled.path)
  } finally {
    await compiled.cleanup()
  }
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

function isManager(value: unknown): value is "bun" | "npm" | "pnpm" {
  return value === "bun" || value === "npm" || value === "pnpm"
}

await main()
