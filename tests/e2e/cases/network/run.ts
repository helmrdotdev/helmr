import { fixtureInput } from "../../support/runtime-mcp"
import { execFile } from "node:child_process"
import { writeFile } from "node:fs/promises"
import { join } from "node:path"
import { fileURLToPath } from "node:url"
import { promisify } from "node:util"
import { verify, assert, assertEqual, waitOutput, completedResult } from "../../support/context"

await verify("metadata-isolation", async ({ marker, computer, startAgent }) => {
  assertEqual(process.env.HELMR_API_URL?.replace(/\/$/, ""), "http://127.0.0.1:58080", "Run on the dedicated runtime host")
  const tool = process.env.HELMR_RUNTIME_HOST_TOOL
  assert(tool, "HELMR_RUNTIME_HOST_TOOL is required")
  const target = await computer("verification", "network")
  const started = await startAgent("verification-network", { computer: target, input: { marker }, idempotencyKey: `${marker}:network` })
  const phase = (name: string) => waitOutput(started.session, started.turn, value => value !== null && typeof value === "object" && "phase" in value && value.phase === name)
  const observe = async (name: string) => {
    const { stdout } = await promisify(execFile)("sudo", ["-n", "python3", fileURLToPath(new URL("./observe.py", import.meta.url)), started.session.id, tool], { timeout: 30_000, maxBuffer: 65536 })
    const result = JSON.parse(stdout) as { runtime_id: string; namespace: string; metadata_in_deny_set: boolean; denied_packets: number }
    await writeFile(join(process.env.HELMR_EVIDENCE_DIR!, `${name}.json`), JSON.stringify(result, null, 2) + "\n", { mode: 0o600 })
    return result
  }
  await phase("ready")
  const before = await observe("network-before")
  await started.turn.send(fixtureInput({ phase: "start" }), { idempotencyKey: `${marker}:start` })
  await phase("observed")
  const after = await observe("network-after")
  assertEqual(after.runtime_id, before.runtime_id, "Network probe changed runtime")
  assertEqual(after.namespace, before.namespace, "Network probe changed namespace")
  assert(before.metadata_in_deny_set && after.metadata_in_deny_set)
  assert(after.denied_packets > before.denied_packets, "No native denied packet observed; timeout alone is insufficient")
  await started.turn.send(fixtureInput({ phase: "finish" }), { idempotencyKey: `${marker}:finish` })
  const output = await completedResult(started.turn, 180_000)
  assertEqual(output, { marker, blocked: true, positiveStatus: 200, turnId: started.turn.id, sessionId: started.session.id, computerId: target.id }, "Network result changed")
  return { before, after, output }
})
