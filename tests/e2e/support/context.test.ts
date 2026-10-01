import { test, expect } from "bun:test"
import { mkdtemp, readFile, rm } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { verify } from "./context"

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

for (const [signal, status] of [["SIGTERM", 143], ["SIGINT", 130]] as const) {
  test(`${signal} preserves owned resources for caller cleanup`, async () => {
    const root = await mkdtemp(join(tmpdir(), "helmr-case-interrupt-"))
    const output = join(root, "evidence")
    const child = Bun.spawn([process.execPath, "-e", `
      import { verify } from ${JSON.stringify(import.meta.dir + "/context.ts")};
      await verify("interrupt", async ({ objects, cleanup }) => {
        objects.computer_ids.push("computer-owned");
        objects.secret_ids.push("secret-owned");
        cleanup(async () => { throw new Error("must not race cleanup with the body"); });
        console.log("ready");
        await new Promise(() => { setInterval(() => {}, 1000); });
      });
    `], {
      env: {
        ...process.env,
        HELMR_API_URL: "http://127.0.0.1:1", HELMR_API_KEY: "fixture",
        HELMR_EVIDENCE_DIR: output, HELMR_EXPECTED_BUNDLE_DIGEST: "",
      },
      stdout: "pipe", stderr: "pipe",
    })
    try {
      const reader = child.stdout.getReader()
      const first = await reader.read()
      expect(new TextDecoder().decode(first.value)).toContain("ready")
      child.kill(signal)
      expect(await child.exited).toBe(status)
      const result = JSON.parse(await readFile(join(output, "result.json"), "utf8"))
      expect(result.passed).toBe(false)
      expect(result.failure).toBe(`Interrupted by ${signal}`)
      expect(result.cleanup.status).toBe("interrupted")
      expect(result.objects.computer_ids).toEqual(["computer-owned"])
      expect(result.objects.secret_ids).toEqual(["secret-owned"])
    } finally {
      child.kill()
      await child.exited
      await rm(root, { recursive: true, force: true })
    }
  })
}
