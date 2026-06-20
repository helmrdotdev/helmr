import { describe, expect, test } from "bun:test"
import { readdir } from "node:fs/promises"
import { basename, resolve } from "node:path"
import { Readable } from "node:stream"
import { fileURLToPath } from "node:url"

import { runAdapterCli, type AdapterIo } from "./main"

const repoRoot = resolve(fileURLToPath(new URL("../../..", import.meta.url)))

interface ParsedRegistry {
  readonly tasks: Record<string, ParsedTask>
}

interface ParsedTask {
  readonly modulePath: string
  readonly exportName: string
  readonly bundle: {
    readonly image?: {
      readonly steps?: readonly Record<string, unknown>[]
    }
    readonly sandbox?: {
      readonly id?: string
      readonly workspace?: {
        readonly mountPath?: string
      }
    }
    readonly task?: {
      readonly id?: string
      readonly modulePath?: string
      readonly exportName?: string
      readonly maxDurationSeconds?: number
    }
  }
}

describe("checked-in task projects parse through the adapter", () => {
  test.each([
    ["examples/cli-tooling", ["cli-tooling"]],
    ["examples/dependency-cache", ["dependency-cache"]],
    ["examples/github-pr-review", ["github-pr-review"]],
    ["examples/hello-world", ["hello-world"]],
    ["examples/human-in-the-loop", ["human-in-the-loop"]],
    ["examples/resend-email-approval", ["resend-email-approval"]],
    ["examples/slack-approval", ["slack-approval"]],
    ["examples/task-secrets", ["use-secret"]],
    [
      "fixtures/task-projects/full-rootfs-runtime",
      [
        "agent-user",
        "alpine-starts",
        "approval",
        "approval-message",
        "approval-restart",
        "contract",
        "default-root",
        "default-workdir-path",
        "distroless-starts",
        "failure-boundary",
        "impl",
        "message",
      ],
    ],
  ])("%s", async (projectPath, expectedTaskIds) => {
    const registry = await parseProject(projectPath)

    expect(Object.keys(registry.tasks)).toEqual(expectedTaskIds)
    for (const [taskId, task] of Object.entries(registry.tasks)) {
      expect(task.bundle.task?.id).toBe(taskId)
      expect(task.bundle.task?.modulePath).toBe(task.modulePath)
      expect(task.bundle.task?.exportName).toBe(task.exportName)
      expect(task.modulePath.startsWith("tasks/")).toBe(true)
      expect(task.exportName).not.toBe("default")
    }
  })

  test("fixtures/task-projects/full-rootfs-runtime preserves the VM contract bundle", async () => {
    const registry = await parseProject("fixtures/task-projects/full-rootfs-runtime")
    const impl = expectTask(registry, "impl")
    const contract = expectTask(registry, "contract")

    expect(impl.bundle.sandbox).toMatchObject({
      id: "full-rootfs-impl",
      workspace: { mountPath: "/workspace" },
    })
    expect(impl.bundle.task).toMatchObject({
      id: "impl",
      modulePath: "tasks/impl.ts",
      exportName: "impl",
      maxDurationSeconds: 900,
    })
    expect(imageStepKinds(impl)).toEqual([
      "from",
      "workdir",
      "copySourceFile",
      "run",
    ])
    expect(impl.bundle.image?.steps?.[2]).toMatchObject({
      copySourceFile: {
        dst: "/opt/helmr-deps/package.json",
        srcRef: { path: "package.json" },
      },
    })
    expect(impl.bundle.image?.steps?.[3]).toMatchObject({
      run: {
        cacheMounts: [
          {
            dst: "/var/cache/helmr-deps",
            cacheId: "full-rootfs-runtime-deps",
          },
        ],
      },
    })

    expect(contract.bundle.sandbox).toMatchObject({
      id: "full-rootfs-contract",
      workspace: { mountPath: "/workspace" },
    })
    expect(imageStepKinds(contract)).toEqual([
      "from",
      "run",
      "copySourceDir",
      "workdir",
      "env",
      "env",
      "env",
    ])
    expect(contract.bundle.image?.steps?.[2]).toMatchObject({
      copySourceDir: {
        dst: "/workspace",
        srcRef: { path: "image-workspace" },
      },
    })
    expect(contract.bundle.image?.steps?.[3]).toMatchObject({
      workdir: { path: "/tmp/task" },
    })
    expect(contract.bundle.image?.steps?.slice(4)).toEqual(
      expect.arrayContaining([
        expect.objectContaining({ env: { key: "FOO", value: "BAR" } }),
        expect.objectContaining({ env: { key: "HOME", value: "/tmp/home-agent" } }),
        expect.objectContaining({
          env: {
            key: "PATH",
            value: "/custom/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
          },
        }),
      ]),
    )
  })

  test("examples/cli-tooling preserves the installed CLI image contract", async () => {
    const registry = await parseProject("examples/cli-tooling")
    const cliTooling = expectTask(registry, "cli-tooling")

    expect(cliTooling.bundle.sandbox).toMatchObject({
      id: "cli-tooling",
      workspace: { mountPath: "/workspace" },
    })
    expect(imageStepKinds(cliTooling)).toEqual([
      "from",
      "workdir",
      "run",
      "run",
      "copySourceFile",
      "workdir",
      "run",
      "workdir",
    ])
    expect(cliTooling.bundle.image?.steps?.[0]).toMatchObject({
      from: { ref: "node:24-bookworm-slim" },
    })
    expect(cliTooling.bundle.image?.steps?.[2]).toMatchObject({
      run: {
        argv: ["npm", "install", "-g", "bun@1.3.10"],
      },
    })
    expect(cliTooling.bundle.image?.steps?.[3]).toMatchObject({
      run: {
        argv: expect.arrayContaining(["sh", "-ceu", expect.stringContaining("ripgrep")]),
      },
    })
    expect(cliTooling.bundle.image?.steps?.[4]).toMatchObject({
      copySourceFile: {
        dst: "/opt/helmr-task/package.json",
        srcRef: { path: "package.json" },
      },
    })
    expect(cliTooling.bundle.image?.steps?.[5]).toMatchObject({
      workdir: { path: "/opt/helmr-task" },
    })
    expect(cliTooling.bundle.image?.steps?.[6]).toMatchObject({
      run: {
        argv: ["bun", "install"],
        cacheMounts: [
          {
            dst: "/root/.bun/install/cache",
            cacheId: "cli-tooling-bun",
          },
        ],
      },
    })
    expect(cliTooling.bundle.image?.steps?.[7]).toMatchObject({
      workdir: { path: "/workspace" },
    })
  })

  test("fixtures and examples discovery stays in sync with the test matrix", async () => {
    expect(await configuredProjectPaths()).toEqual([
      "examples/cli-tooling",
      "examples/dependency-cache",
      "examples/github-pr-review",
      "examples/hello-world",
      "examples/human-in-the-loop",
      "examples/resend-email-approval",
      "examples/slack-approval",
      "examples/task-secrets",
      "fixtures/task-projects/full-rootfs-runtime",
    ])
  })
})

async function configuredProjectPaths(): Promise<string[]> {
  const exampleNames = await readdir(resolve(repoRoot, "examples"))
  const fixtureNames = await readdir(resolve(repoRoot, "fixtures/task-projects"))
  return [
    ...exampleNames
      .filter((name) => name !== "README.md")
      .map((name) => `examples/${name}`),
    ...fixtureNames.map((name) => `fixtures/task-projects/${name}`),
  ].sort()
}

async function parseProject(projectPath: string): Promise<ParsedRegistry> {
  const result = await invokeAdapter(["parse", "--cwd", resolve(repoRoot, projectPath), "--output", "json"])
  expect(result.status, result.stderr).toBe(0)
  return JSON.parse(result.stdout) as ParsedRegistry
}

function expectTask(registry: ParsedRegistry, taskId: string): ParsedTask {
  const task = registry.tasks[taskId]
  if (task === undefined) {
    throw new Error(`${taskId} task should be present`)
  }
  return task
}

function imageStepKinds(task: ParsedTask): string[] {
  return task.bundle.image?.steps?.map((step) => {
    const keys = Object.keys(step)
    expect(keys, `expected exactly one image step kind in ${basename(task.modulePath)}`).toHaveLength(1)
    return keys[0] ?? ""
  }) ?? []
}

async function invokeAdapter(
  argv: readonly string[],
): Promise<{ readonly stdout: string; readonly stderr: string; readonly status: number }> {
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
