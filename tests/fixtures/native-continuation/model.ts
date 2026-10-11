// Standalone synthetic provider for the online-save/process-loss fixture.
import { readFile, writeFile } from "node:fs/promises"
import { startNativeModels } from "./model-server"
process.stderr.write("synthetic model process started\n")
const saved = process.argv[4] ? JSON.parse(await readFile(process.argv[4], "utf8")) : undefined
const ports = saved?.ModelConfig
if (saved && (!ports || ["codex", "claude", "proof"].some(name => !Number.isInteger(ports[name]) || ports[name] <= 1024 || ports[name] > 65535))) throw new Error("Saved model ports are incomplete")
const model = await startNativeModels(() => process.env.HELMR_NATIVE_TARGET_SESSION ?? "peer", ports, saved?.ModelHistory)
await writeFile(process.argv[2]!, JSON.stringify({ ...model.endpoints, host: process.argv[3] ?? "127.0.0.1" }))
process.stderr.write("synthetic model endpoints ready\n")
