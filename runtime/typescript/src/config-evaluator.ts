import { canonicalizeJsonValue, type JsonValue } from "@helmr/sdk/internal"
import { createWriteStream } from "node:fs"
import { loadConfig } from "./config"

const maxConfigBytes = 1 << 20

async function main(): Promise<void> {
  if (process.argv.length !== 3 || process.argv[2] === undefined) {
    throw new Error("Config Evaluator requires exactly one Program root")
  }
  const body = canonicalizeJsonValue(
    await loadConfig(process.argv[2]) as unknown as JsonValue,
  )
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
