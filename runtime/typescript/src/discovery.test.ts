import { describe, expect, test } from "bun:test"
import { mkdir, mkdtemp, stat, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { resolve } from "node:path"
import { Readable } from "node:stream"
import { fileURLToPath } from "node:url"

import { runAdapterCli, type AdapterIo } from "./main"

describe("task registry from helmr.config.ts dirs", () => {
  test("inspect-config emits project and dirs without discovering tasks", async () => {
    const cwd = await mkdtemp(resolve(tmpdir(), "helmr-discovery-inspect-config-"))
    await mkdir(resolve(cwd, "tasks"), { recursive: true })
    await writeFile(
      resolve(cwd, "helmr.config.ts"),
      'import { defineConfig } from "@helmr/sdk"\nexport default defineConfig({ project: "local-deploys", dirs: ["./tasks"] })\n',
    )

    const result = await invokeAdapter(["inspect-config", "--cwd", cwd])
    expect(result.status, result.stderr).toBe(0)
    expect(JSON.parse(result.stdout)).toEqual({
      project: "local-deploys",
      dirs: ["./tasks"],
      ignorePatterns: null,
    })
  })

  test("inspect-config emits configured ignore patterns", async () => {
    const cwd = await mkdtemp(resolve(tmpdir(), "helmr-discovery-inspect-ignore-"))
    await writeFile(
      resolve(cwd, "helmr.config.ts"),
      'import { defineConfig } from "@helmr/sdk"\nexport default defineConfig({ dirs: ["./tasks"], ignorePatterns: ["secrets/**"] })\n',
    )

    const result = await invokeAdapter(["inspect-config", "--cwd", cwd])
    expect(result.status, result.stderr).toBe(0)
    expect(JSON.parse(result.stdout)).toEqual({
      project: null,
      dirs: ["./tasks"],
      ignorePatterns: ["secrets/**"],
    })
  })

  test("inspect-config includes null project when omitted", async () => {
    const cwd = await mkdtemp(resolve(tmpdir(), "helmr-discovery-inspect-config-no-project-"))
    await writeFile(
      resolve(cwd, "helmr.config.ts"),
      'import { defineConfig } from "@helmr/sdk"\nexport default defineConfig({ dirs: ["./missing"] })\n',
    )

    const result = await invokeAdapter(["inspect-config", "--cwd", cwd])
    expect(result.status, result.stderr).toBe(0)
    expect(JSON.parse(result.stdout)).toEqual({
      project: null,
      dirs: ["./missing"],
      ignorePatterns: null,
    })
  })

  test("missing helmr.config.ts fails fast", async () => {
    const cwd = await mkdtemp(resolve(tmpdir(), "helmr-discovery-missing-config-"))
    const result = await invokeAdapter(["parse", "--cwd", cwd, "--output", "json"])

    expect(result.status).toBe(1)
    const error = JSON.parse(result.stderr)
    expect(error).toMatchObject({ level: "error", kind: "missing_config" })
    expect(error.message).toContain("helmr.config.ts")
  })

  test("missing, empty, or unmatched dirs fail fast", async () => {
    const missing = await mkdtemp(resolve(tmpdir(), "helmr-discovery-missing-dirs-"))
    await writeFile(
      resolve(missing, "helmr.config.ts"),
      'import { defineConfig } from "@helmr/sdk"\nexport default defineConfig({} as never)\n',
    )
    const missingResult = await invokeAdapter(["parse", "--cwd", missing, "--output", "json"])
    expect(missingResult.status).toBe(1)
    expect(JSON.parse(missingResult.stderr).message).toContain("requires a non-empty dirs array")

    const empty = await mkdtemp(resolve(tmpdir(), "helmr-discovery-empty-dirs-"))
    await writeFile(
      resolve(empty, "helmr.config.ts"),
      'import { defineConfig } from "@helmr/sdk"\nexport default defineConfig({ dirs: [] })\n',
    )
    const emptyResult = await invokeAdapter(["parse", "--cwd", empty, "--output", "json"])
    expect(emptyResult.status).toBe(1)
    expect(JSON.parse(emptyResult.stderr).message).toContain("requires a non-empty dirs array")

    const noFiles = await mkdtemp(resolve(tmpdir(), "helmr-discovery-no-files-"))
    await mkdir(resolve(noFiles, "tasks"), { recursive: true })
    await writeFile(
      resolve(noFiles, "helmr.config.ts"),
      'import { defineConfig } from "@helmr/sdk"\nexport default defineConfig({ dirs: ["./tasks"] })\n',
    )
    const noFilesResult = await invokeAdapter(["parse", "--cwd", noFiles, "--output", "json"])
    expect(noFilesResult.status).toBe(1)
    expect(JSON.parse(noFilesResult.stderr).message).toContain("no task files found")
  })

  test("duplicate task ids fail with both origin files", async () => {
    const cwd = await mkdtemp(resolve(tmpdir(), "helmr-discovery-duplicate-"))
    await mkdir(resolve(cwd, "tasks/nested"), { recursive: true })
    await writeTask(cwd, "tasks/one.ts", "dup")
    await writeTask(cwd, "tasks/nested/two.ts", "dup")
    await writeFile(
      resolve(cwd, "helmr.config.ts"),
      'import { defineConfig } from "@helmr/sdk"\nexport default defineConfig({ dirs: ["./tasks"] })\n',
    )

    const result = await invokeAdapter(["parse", "--cwd", cwd, "--output", "json"])
    expect(result.status).toBe(1)
    const error = JSON.parse(result.stderr)
    expect(error).toMatchObject({ level: "error", kind: "duplicate_task_id" })
    expect(error.message).toContain('duplicate task id "dup"')
    expect(error.message).toContain("tasks/one.ts")
    expect(error.message).toContain("tasks/nested/two.ts")
  })

  test("ignored helper modules are not imported", async () => {
    const cwd = await mkdtemp(resolve(tmpdir(), "helmr-discovery-utility-"))
    await mkdir(resolve(cwd, "tasks"), { recursive: true })
    await writeTask(cwd, "tasks/task.ts", "reachable")
    await writeFile(
      resolve(cwd, "tasks/_utils.ts"),
      `await Bun.write(${JSON.stringify(resolve(cwd, "utility-spy.txt"))}, "imported")\n`,
    )
    await writeFile(
      resolve(cwd, "helmr.config.ts"),
      'import { defineConfig } from "@helmr/sdk"\nexport default defineConfig({ dirs: ["./tasks"] })\n',
    )

    const result = await invokeAdapter(["parse", "--cwd", cwd, "--output", "json"])
    expect(result.status, result.stderr).toBe(0)
    await expect(stat(resolve(cwd, "utility-spy.txt"))).rejects.toThrow()
  })

  test("task run body is not invoked while building the registry", async () => {
    const cwd = await mkdtemp(resolve(tmpdir(), "helmr-discovery-run-body-"))
    await mkdir(resolve(cwd, "tasks"), { recursive: true })
    await writeFile(
      resolve(cwd, "tasks/task.ts"),
      `import { image, sandbox, task } from "@helmr/sdk"
const sb = sandbox("spy").image(image("spy").from("debian:trixie-slim")).workspace("/app")
export const spy = task({
  id: "spy",
  sandbox: sb,
  run: async () => {
    await Bun.write(${JSON.stringify(resolve(cwd, "run-spy.txt"))}, "invoked")
    return { ok: true }
  },
})
`,
    )
    await writeFile(
      resolve(cwd, "helmr.config.ts"),
      'import { defineConfig } from "@helmr/sdk"\nexport default defineConfig({ dirs: ["./tasks"] })\n',
    )

    const result = await invokeAdapter(["parse", "--cwd", cwd, "--task", "spy", "--output", "binary"])
    expect(result.status, result.stderr).toBe(0)
    await expect(stat(resolve(cwd, "run-spy.txt"))).rejects.toThrow()
  })
})

async function writeTask(cwd: string, path: string, id: string): Promise<void> {
  await writeFile(
    resolve(cwd, path),
    `import { image, sandbox, task } from "@helmr/sdk"
const sb = sandbox(${JSON.stringify(id)}).image(image(${JSON.stringify(id)}).from("debian:trixie-slim")).workspace("/app")
export const discoveredTask = task({ id: ${JSON.stringify(id)}, sandbox: sb, run: async () => ({ ok: true }) })
`,
  )
}

async function invokeAdapter(
  argv: readonly string[],
): Promise<{ readonly stdout: string; readonly stderr: string; readonly status: number }> {
  const previousAdapterSdkPath = process.env["HELMR_ADAPTER_SDK_PATH"]
  process.env["HELMR_ADAPTER_SDK_PATH"] = fileURLToPath(
    new URL("../../../sdk/typescript/src/index.ts", import.meta.url),
  )
  const stdout = new CaptureSink()
  const stderr = new CaptureSink()
  const io: AdapterIo = {
    stdin: Readable.from([]),
    stdout,
    stderr,
  }
  try {
    const status = await runAdapterCli(argv, io)
    return { stdout: stdout.text(), stderr: stderr.text(), status }
  } finally {
    if (previousAdapterSdkPath === undefined) {
      delete process.env["HELMR_ADAPTER_SDK_PATH"]
    } else {
      process.env["HELMR_ADAPTER_SDK_PATH"] = previousAdapterSdkPath
    }
  }
}

class CaptureSink {
  readonly #chunks: Buffer[] = []

  write(chunk: string | Uint8Array): boolean {
    this.#chunks.push(typeof chunk === "string" ? Buffer.from(chunk) : Buffer.from(chunk))
    return true
  }

  text(): string {
    return Buffer.concat(this.#chunks).toString()
  }
}
