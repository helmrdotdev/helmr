import { test, expect } from "bun:test"
import { mkdtemp, readFile, rm } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { verify } from "../e2e/support/context"

// Failure evidence must survive a broken cleanup action, and later cleanup must run.
test("case failure keeps its verdict and attempts every registered cleanup", async () => {
  const root = await mkdtemp(join(tmpdir(), "helmr-case-"))
  const old = { ...process.env }
  const calls: string[] = []
  try {
    process.env.HELMR_API_URL = "http://127.0.0.1:1"
    process.env.HELMR_API_KEY = "fixture"
    process.env.HELMR_EVIDENCE_DIR = join(root, "evidence")
    delete process.env.HELMR_EXPECTED_BUNDLE_DIGEST
    await expect(verify("failure", async ({ cleanup }) => {
      cleanup(async () => { calls.push("first") })
      cleanup(async () => { calls.push("second"); throw new Error("cleanup failed") })
      throw new Error("assertion failed")
    })).rejects.toThrow("assertion failed")
    expect(calls).toEqual(["second", "first"])
    const result = JSON.parse(await readFile(join(root, "evidence/result.json"), "utf8"))
    expect(result.passed).toBe(false)
    expect(result.failure).toBe("assertion failed")
    expect(result.cleanup).toEqual({ status: "failed", failures: ["cleanup failed"] })
    let started = false
    await expect(verify("overwrite", async () => { started = true })).rejects.toThrow()
    expect(started).toBe(false)
  } finally {
    for (const key of ["HELMR_API_URL", "HELMR_API_KEY", "HELMR_EVIDENCE_DIR", "HELMR_EXPECTED_BUNDLE_DIGEST"]) {
      if (old[key] === undefined) delete process.env[key]
      else process.env[key] = old[key]
    }
    await rm(root, { recursive: true, force: true })
  }
})
