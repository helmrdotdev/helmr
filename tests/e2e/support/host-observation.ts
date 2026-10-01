import { execFile } from "node:child_process"
import { promisify } from "node:util"
import { assert } from "./context"

export async function hostObservation(name: string, inputs: Record<string, unknown>): Promise<any> {
  assert.equal(process.env.HELMR_API_URL?.replace(/\/$/, ""), "http://127.0.0.1:58080")
  const tool = process.env.HELMR_RUNTIME_HOST_TOOL
  assert(tool, "HELMR_RUNTIME_HOST_TOOL must identify the installed Product profile")
  const { stdout } = await promisify(execFile)("sudo", ["-n", "python3", tool,
    "observe", "--observation", name, "--inputs", JSON.stringify(inputs)],
    { timeout: 15_000, maxBuffer: 262144 })
  return JSON.parse(stdout)
}
