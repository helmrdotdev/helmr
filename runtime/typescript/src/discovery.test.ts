import { describe, expect, test } from "bun:test"
import { mkdir, mkdtemp, stat, symlink, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { dirname, resolve } from "node:path"
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
      `import { writeFile } from "node:fs/promises"\nawait writeFile(${JSON.stringify(resolve(cwd, "utility-spy.txt"))}, "imported")\n`,
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
    const { writeFile } = await import("node:fs/promises")
    await writeFile(${JSON.stringify(resolve(cwd, "run-spy.txt"))}, "invoked")
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

  test("task module imports resolve packages from the project root", async () => {
    const cwd = await mkdtemp(resolve(tmpdir(), "helmr-discovery-package-root-"))
    await mkdir(resolve(cwd, "tasks"), { recursive: true })
    await mkdir(resolve(cwd, "node_modules/package-root-probe/lib"), { recursive: true })
    await writeFile(
      resolve(cwd, "node_modules/package-root-probe/package.json"),
      '{"name":"package-root-probe","type":"module","main":"./index.js"}\n',
    )
    await writeFile(
      resolve(cwd, "node_modules/package-root-probe/index.js"),
      'export { marker } from "./lib/probe.js"\n',
    )
    await writeFile(
      resolve(cwd, "node_modules/package-root-probe/lib/probe.js"),
      `import { readFile } from "node:fs/promises"
const metadata = JSON.parse(await readFile(new URL("../package.json", import.meta.url), "utf8"))
if (metadata.name !== "package-root-probe") throw new Error("package root not found")
export const marker = "ok"
`,
    )
    await writeFile(
      resolve(cwd, "tasks/task.ts"),
      `import { image, sandbox, task } from "@helmr/sdk"
import { marker } from "package-root-probe"
const sb = sandbox("package-root").image(image("package-root").from("debian:trixie-slim")).workspace("/app")
export const packageRoot = task({ id: "package-root-" + marker, sandbox: sb, run: async () => ({ ok: true }) })
`,
    )
    await writeFile(
      resolve(cwd, "helmr.config.ts"),
      'import { defineConfig } from "@helmr/sdk"\nexport default defineConfig({ dirs: ["./tasks"] })\n',
    )

    const result = await invokeAdapter(["parse", "--cwd", cwd, "--output", "json"])
    expect(result.status, result.stderr).toBe(0)
    expect(JSON.parse(result.stdout).tasks).toHaveProperty("package-root-ok")
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
  const sdkRoot = fileURLToPath(new URL("../../../sdk/typescript", import.meta.url))
  const cwd = optionValue(argv, "--cwd")
  if (cwd !== undefined) {
    await linkLocalSdk(resolve(cwd), sdkRoot)
  }
  const stdout = new CaptureSink()
  const stderr = new CaptureSink()
  const io: AdapterIo = {
    stdin: Readable.from([]),
    stdout,
    stderr,
  }
  const status = await runAdapterCli(argv, io)
  return { stdout: stdout.text(), stderr: stderr.text(), status }
}

function optionValue(argv: readonly string[], name: string): string | undefined {
  const index = argv.indexOf(name)
  return index === -1 ? undefined : argv[index + 1]
}

async function linkLocalSdk(cwd: string, sdkRoot: string): Promise<void> {
  const packagePath = resolve(cwd, "package.json")
  try {
    await stat(packagePath)
  } catch (error) {
    if ((error as NodeJS.ErrnoException | undefined)?.code !== "ENOENT") {
      throw error
    }
    await writeFile(packagePath, '{"private":true,"type":"module","dependencies":{"@helmr/sdk":"latest"}}\n')
  }
  const linkPath = resolve(cwd, "node_modules/@helmr/sdk")
  await mkdir(dirname(linkPath), { recursive: true })
  try {
    await symlink(sdkRoot, linkPath, "dir")
  } catch (error) {
    if ((error as NodeJS.ErrnoException | undefined)?.code !== "EEXIST") {
      throw error
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
