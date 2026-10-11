import { execFile } from "node:child_process"
import { writeFile } from "node:fs/promises"
import { join } from "node:path"
import { promisify } from "node:util"
import { assert, assertEqual } from "./context"

// Read this dedicated host's authoritative checkpoint/lease evidence and retain
// each observation immediately, including when a later case assertion fails.
export async function observePersistence(action: "wait-parked" | "verify-restored" | "wait-aborted", sessionId: string) {
  assertEqual(process.env.HELMR_API_URL?.replace(/\/$/, ""), "http://127.0.0.1:58080", "Run the physical observer on the dedicated runtime host")
  const hostTool = process.env.HELMR_RUNTIME_HOST_TOOL, evidenceDir = process.env.HELMR_EVIDENCE_DIR
  assert(hostTool && evidenceDir, "Host tool and evidence directory are required")
  const { stdout } = await promisify(execFile)("sudo", ["-n", "python3", hostTool, action, "--session-id", sessionId], { timeout: 200_000, maxBuffer: 65536 })
  const observation = JSON.parse(stdout) as Record<string, unknown>
  await writeFile(join(evidenceDir, `${sessionId}-${action}.json`), JSON.stringify(observation, null, 2) + "\n", { mode: 0o600 })
  return observation
}

export function assertRestored(parked: Record<string, unknown>, restored: Record<string, unknown>, sessionId: string) {
  for (const value of [parked, restored]) {
    assertEqual(value.session_id, sessionId, "Checkpoint belongs to another Session")
    assertEqual(value.source_fenced, true, "Source lease was not fenced")
  }
  assertEqual(parked.checkpoint_status, "ready", "Checkpoint was not durable before resume")
  assertEqual(parked.target_lease_epoch, null, "Computer was already restoring before resume")
  assertEqual(restored.checkpoint_id, parked.checkpoint_id, "Session used a different checkpoint")
  assertEqual(restored.prior_runtime_id, parked.prior_runtime_id, "Source runtime changed")
  assertEqual(restored.source_lease_epoch, parked.source_lease_epoch, "Source lease changed")
  assertEqual(restored.checkpoint_status, "consumed", "Checkpoint was not consumed")
  assertEqual(restored.controls_reconciled, true, "Restore omitted control reconciliation")
  assertEqual(restored.target_current, true, "Restored Session is not on the current healthy lease")
  assert(typeof restored.target_runtime_id === "string" && restored.target_runtime_id !== parked.prior_runtime_id, "Session did not move to a new runtime")
  assert(typeof restored.source_lease_epoch === "number" && typeof restored.target_lease_epoch === "number" && restored.target_lease_epoch > restored.source_lease_epoch, "Restore did not advance the lease epoch")
}
