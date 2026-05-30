import { describe, expect, test } from "bun:test"
import { create, fromBinary, toBinary } from "@bufbuild/protobuf"
import { createHash } from "node:crypto"
import { mkdir, mkdtemp, realpath, symlink, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { dirname, resolve } from "node:path"
import { PassThrough, Readable } from "node:stream"
import { fileURLToPath } from "node:url"

import {
  BundleSchema,
  ImageStepSchema,
  PlatformSchema,
  type ImageSpec,
  type ImageStep,
  runProto,
} from "@helmr/proto"
import {
  runAdapterCli,
  type AdapterIo,
} from "../../../runtime/typescript/src/main"
import { compile } from "./compile"
import { image, sandbox, source, task, type PayloadSchema } from "./index"

describe("compile", () => {
  test("emits a stable bundle from the compile fixture", async () => {
    const bundle = compile({
      task: compileFixtureTask(),
      modulePath: "tasks/hello.ts",
    })

    const image = expectPresent(bundle.image, "ImageSpec")
    const sandbox = expectPresent(bundle.sandbox, "sandbox spec")
    const task = expectPresent(bundle.task, "task spec")

    expect(image.steps.map((step) => step.kind.case)).toEqual([
      "from",
      "run",
      "copySourceDir",
      "workdir",
      "env",
      "user",
    ])
    expect(image.formatVersion).toBe(0)
    expect(sandbox.workspace?.mountPath).toBe("/app")
    expect(task.sandboxId).toBe("compile-fixture")

    const from = image.steps[0]?.kind
    expect(from).toMatchObject({ case: "from", value: { ref: "debian:trixie-slim" } })

    const depsSource = image.steps[2]?.kind
    expect(depsSource).toMatchObject({
      case: "copySourceDir",
      value: {
        dst: "/app",
        srcRef: { path: ".", ignore: ["*", "!package.json", "!bun.lock", "!tsconfig.json"] },
      },
    })

  })

  test("rejects missing image declarations", () => {
    expect(() =>
      compile({
        task: task({
          id: "missing-image",
          sandbox: sandbox("test").workspace("/app"),
          run: async () => undefined,
        }),
        modulePath: "tasks/missing-image.ts",
      }),
    ).toThrow('sandbox "test" must declare image(...)')

  })

  test("rejects empty image chains", () => {
    expect(() =>
      compile({
        task: task({
          id: "empty-image",
          sandbox: sandbox("test").image(image("empty")).workspace("/app"),
          run: async () => undefined,
        }),
        modulePath: "tasks/empty-image.ts",
      }),
    ).toThrow('image "empty" must contain at least one operation')
  })

  test("task maxDuration accepts bounds and rejects values outside them", () => {
    const base = image("duration").from("debian:trixie-slim")
    const smokeSandbox = sandbox("duration").image(base)
    expect(() =>
      task({ id: "too-short", sandbox: smokeSandbox, maxDuration: 4, run: async () => null }),
    ).toThrow(/maxDuration/)
    expect(() =>
      task({ id: "too-long", sandbox: smokeSandbox, maxDuration: 86401, run: async () => null }),
    ).toThrow(/maxDuration/)

    const bundle = compile({
      task: task({
        id: "maximum",
        sandbox: smokeSandbox,
        maxDuration: 86400,
        run: async () => null,
      }),
      modulePath: "tasks/maximum.ts",
    })
    expect(bundle.task?.maxDurationSeconds).toBe(86400)
  })

  test("task maxDuration defaults to explicit seconds in bundle", () => {
    const bundle = compile({
      task: task({
        id: "default-duration",
        sandbox: sandbox("duration-default").image(image("duration-default").from("debian:trixie-slim")),
        run: async () => null,
      }),
      modulePath: "tasks/default-duration.ts",
    })
    expect(bundle.task?.maxDurationSeconds).toBe(900)
  })

  test("emits payload schema metadata when available", () => {
    const payloadSchema: PayloadSchema<unknown> = {
      "~standard": {
        version: 1,
        vendor: "test",
        validate(value) {
          return { value }
        },
      },
      toJSONSchema() {
        return {
          type: "object",
          required: ["branch"],
          properties: {
            branch: { type: "string" },
          },
        }
      },
    }
    const bundle = compile({
      task: task({
        id: "schema-metadata",
        sandbox: sandbox("schema-metadata").image(image("schema-metadata").from("debian:trixie-slim")),
        payloadSchema,
        run: async (payload) => payload,
      }),
      modulePath: "tasks/schema-metadata.ts",
    })

    expect(JSON.parse(bundle.task?.payloadSchemaJson ?? "")).toEqual({
      type: "object",
      required: ["branch"],
      properties: {
        branch: { type: "string" },
      },
    })
  })

  test("rejects payload schema metadata that is not a JSON Schema object or boolean", () => {
    const payloadSchema: PayloadSchema<unknown> = {
      "~standard": {
        version: 1,
        vendor: "test",
        validate(value) {
          return { value }
        },
      },
      toJSONSchema() {
        return null
      },
    }

    expect(() =>
      compile({
        task: task({
          id: "bad-schema-metadata",
          sandbox: sandbox("bad-schema-metadata").image(image("bad-schema-metadata").from("debian:trixie-slim")),
          payloadSchema,
          run: async (payload) => payload,
        }),
        modulePath: "tasks/bad-schema-metadata.ts",
      }),
    ).toThrow("payloadSchema.toJSONSchema() must return a JSON Schema object or boolean")
  })

  test("rejects malformed secret placements during compile", () => {
    expect(() =>
      compile({
        task: task({
          id: "bad-secret",
          sandbox: sandbox("test")
            .image(image("test").from("debian:trixie-slim"))
            .workspace("/app"),
          secrets: {
            broken: { env: "TOKEN", file: "/tmp/secret" } as never,
          },
          run: async () => undefined,
        }),
        modulePath: "tasks/bad-secret.ts",
      }),
    ).toThrow("task secrets.broken must be { env: string }")
  })

  test("rejects blank secret placement targets during compile", () => {
    const base = {
      id: "blank-secret-target",
      sandbox: sandbox("test")
        .image(image("test").from("debian:trixie-slim"))
        .workspace("/app"),
      run: async () => undefined,
    }
    expect(() =>
      compile({
        task: task({
          ...base,
          secrets: {
            token: { env: " " },
          },
        }),
        modulePath: "tasks/blank-secret-target.ts",
      }),
    ).toThrow("task secrets.token must be { env: string }")
    expect(() =>
      compile({
        task: task({
          ...base,
          secrets: {
            token: { file: "" },
          },
        }),
        modulePath: "tasks/blank-secret-target.ts",
      }),
    ).toThrow("task secrets.token must be { file: string, mode?: string, owner?: string }")
    expect(() =>
      compile({
        task: task({
          ...base,
          secrets: {
            token: { dir: "\t" },
          },
        }),
        modulePath: "tasks/blank-secret-target.ts",
      }),
    ).toThrow("task secrets.token must be { dir: string, mode?: string, owner?: string }")
  })

  test("rejects unsafe secret placement paths during compile", () => {
    const base = {
      id: "unsafe-secret-target",
      sandbox: sandbox("test")
        .image(image("test").from("debian:trixie-slim"))
        .workspace("/app"),
      run: async () => undefined,
    }
    expect(() =>
      compile({
        task: task({
          ...base,
          secrets: {
            token: { file: "a/../secret" },
          },
        }),
        modulePath: "tasks/unsafe-secret-target.ts",
      }),
    ).toThrow("task secrets.token.file must not contain parent components")
    expect(() =>
      compile({
        task: task({
          ...base,
          secrets: {
            token: { dir: "/" },
          },
        }),
        modulePath: "tasks/unsafe-secret-target.ts",
      }),
    ).toThrow("task secrets.token.dir must target a file or directory")
    expect(() =>
      compile({
        task: task({
          ...base,
          secrets: {
            token: { file: " /tmp/secret" },
          },
        }),
        modulePath: "tasks/unsafe-secret-target.ts",
      }),
    ).toThrow("task secrets.token.file must not contain leading or trailing whitespace")
  })

  test("rejects invalid secret placement modes during compile", () => {
    const base = {
      id: "bad-secret-mode",
      sandbox: sandbox("test")
        .image(image("test").from("debian:trixie-slim"))
        .workspace("/app"),
      run: async () => undefined,
    }
    expect(() =>
      compile({
        task: task({
          ...base,
          secrets: {
            token: { file: "/tmp/secret", mode: "not-octal" },
          },
        }),
        modulePath: "tasks/bad-secret-mode.ts",
      }),
    ).toThrow("task secrets.token.mode must be an octal permission mode")
    expect(() =>
      compile({
        task: task({
          ...base,
          secrets: {
            token: { dir: "/tmp/secrets", mode: "1777" },
          },
        }),
        modulePath: "tasks/bad-secret-mode.ts",
      }),
    ).toThrow("task secrets.token.mode must only contain permission bits")
  })

  test("binary bundle round-trips through protobuf", async () => {
    const bundle = compile({ task: compileFixtureTask(), modulePath: "tasks/hello.ts" })

    const decoded = fromBinary(BundleSchema, toBinary(BundleSchema, bundle))

    expect(decoded.task?.id).toBe("hello")
    expect(decoded.image?.steps.length).toBe(bundle.image?.steps.length)
    expect(decoded.sandbox?.id).toBe("compile-fixture")
  })

  test("provisional sub-image key resolves compile-time sub-image references", () => {
    const staticTool = image("static-tool").from("debian:trixie-slim").run(["echo", "tool"])
    const sourceTool = image("source-tool")
      .from("debian:trixie-slim")
      .copy("/usr/local/bin/tool", source.file("tool.sh"))
    const app = image("app")
      .from("debian:trixie-slim")
      .copy("/static-tool", staticTool)
      .copy("/source-tool", sourceTool)
    const bundle = compile({
      task: task({
        id: "sub-image-static-key",
        sandbox: sandbox("test").image(app).workspace("/app"),
        run: async () => undefined,
      }),
      modulePath: "tasks/sub-image.ts",
    })
    const staticKey = expectPresent(
      bundle.image?.steps[1]?.kind.case === "copyFromImage"
        ? bundle.image.steps[1].kind.value.srcImageKey
        : undefined,
      "static sub-image key",
    )
    const sourceKey = expectPresent(
      bundle.image?.steps[2]?.kind.case === "copyFromImage"
        ? bundle.image.steps[2].kind.value.srcImageKey
        : undefined,
      "source sub-image key",
    )

    expect(bundle.subImages[staticKey]?.steps.map((step) => step.kind.case)).toEqual(["from", "run"])
    const sourceSubImage = expectPresent(bundle.subImages[sourceKey], "source sub-image spec")
    expect(sourceSubImage.steps[1]?.kind).toMatchObject({
      case: "copySourceFile",
      value: { srcRef: { path: "tool.sh" }, digest: "" },
    })
  })

  test("image_key golden byte layout matches source-only fixture", () => {
    const tool = image("tool").from("debian:trixie-slim")
    const app = image("golden")
      .from(
        "debian:trixie-slim@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
      )
      .copy("/app/package.json", source.file("package.json"))
      .copy("/tool", tool)
    const bundle = compile({
      task: task({
        id: "golden",
        sandbox: sandbox("test").image(app).workspace("/workspace"),
        run: async () => undefined,
      }),
      modulePath: "tasks/golden.ts",
    })
    const compiledImage = expectPresent(bundle.image, "ImageSpec")
    if (compiledImage.platform) {
      compiledImage.platform.architecture = "arm64"
    }
    const copySource = expectPresent(compiledImage.steps[1]?.kind, "copy source step")
    const copyImage = expectPresent(compiledImage.steps[2]?.kind, "copy image step")
    expect(copySource.case).toBe("copySourceFile")
    expect(copyImage.case).toBe("copyFromImage")
    if (copySource.case === "copySourceFile") {
      copySource.value.digest =
        "sha256:1111111111111111111111111111111111111111111111111111111111111111"
    }
    if (copyImage.case === "copyFromImage") {
      copyImage.value.srcImageKey =
        "sha256:2222222222222222222222222222222222222222222222222222222222222222"
      copyImage.value.srcPath = "/bin/tool"
    }

    expect(tsImageKey(compiledImage)).toBe(
      "sha256:b761f2a294723b3c7186aaa22632d84389a3dbc440b5c3293e46988e3eb28729",
    )
  })

  test("sandbox workspace mount path is normalized", () => {
    const bundle = compile({
      task: task({
        id: "workspace-normalized",
        sandbox: sandbox("workspace-normalized")
          .image(image("workspace-normalized").from("debian:trixie-slim"))
          .workspace("/workspace/./project"),
        run: async () => undefined,
      }),
      modulePath: "tasks/workspace-normalized.ts",
    })

    expect(bundle.sandbox?.workspace?.mountPath).toBe("/workspace/project")
  })

  test("sandbox workspace rejects unsafe mount paths", () => {
    for (const mountPath of [
      "workspace",
      "/",
      "/../../etc",
      "/workspace/../etc",
      "/dev",
      "/dev/shm",
      "/opt/helmr",
      "/proc",
      "/run",
      "/sys",
      "/tmp",
      "/.helmr-old-root",
    ]) {
      expect(() => sandbox("bad-workspace").workspace(mountPath)).toThrow(/mountPath/)
    }
  })

  test("adapter --output binary emits protobuf bytes on stdout", async () => {
    const cwd = await parseTaskFixture(
      "hello",
      `
import { task } from "@helmr/sdk"
import { smokeSandbox } from "../fixture/sandbox"

export const hello = task({
  id: "hello",
  sandbox: smokeSandbox,
  run: async () => ({ ok: true }),
})
`,
    )
    const result = await invokeAdapter([
      "parse",
      "--cwd",
      cwd,
      "--task",
      "hello",
      "--output",
      "binary",
    ])

    expect(result.status, result.stderr).toBe(0)
    const decoded = fromBinary(BundleSchema, result.stdout)
    expect(decoded.task?.id).toBe("hello")
    expect(decoded.sandbox?.id).toBe("hello")
  })

  test("adapter run emits task output on the control channel", async () => {
    const cwd = await taskFixture("hello", "({ ok: true })")
    const result = await runAdapterTask(cwd, "hello")

    expect(result.status, result.stderr).toBe(0)
    expect(result.stdout).toBe("")
    expect(taskOutput(result)).toEqual({ ok: true })
  })

  test("adapter run loads tasks from task cwd while process cwd stays workspace", async () => {
    const taskCwd = await taskFixture(
      "hello",
      `({ cwd: process.cwd(), workspace: await (await import("node:fs/promises")).readFile("workspace.txt", "utf8") })`,
    )
    const workspaceCwd = await realpath(await mkdtemp(resolve(tmpdir(), "helmr-adapter-workspace-test-")))
    await writeFile(resolve(workspaceCwd, "workspace.txt"), "workspace-data")
    const result = await runAdapterTask(workspaceCwd, "hello", { taskCwd })

    expect(result.status, result.stderr).toBe(0)
    expect(taskOutput(result)).toEqual({
      cwd: workspaceCwd,
      workspace: "workspace-data",
    })
  })

  test("adapter run passes payload JSON and run id into task context", async () => {
    const cwd = await parseTaskFixture(
      "payload",
      `import { smokeSandbox } from "../fixture/sandbox"
import { task, type PayloadSchema } from "@helmr/sdk"

const payloadSchema: PayloadSchema<{ readonly branch: string; readonly attempts: number }> = {
  "~standard": {
    version: 1,
    vendor: "test",
    validate(value) {
      return { value: value as { readonly branch: string; readonly attempts: number } }
    },
  },
  toJSONSchema() {
    return {}
  },
}

export const payload = task({
  id: "payload",
  sandbox: smokeSandbox,
  payloadSchema,
  run: async (payload, ctx) => ({ payload, runId: ctx.run.id }),
})
`,
    )
    const result = await runAdapterTask(cwd, "payload", {
      runId: "run-custom",
      payloadJson: JSON.stringify({ branch: "main", attempts: 2 }),
    })

    expect(result.status, result.stderr).toBe(0)
    expect(taskOutput(result)).toEqual({
      payload: { branch: "main", attempts: 2 },
      runId: "run-custom",
    })
  })

  test("adapter run parses payloadSchema before invoking task code", async () => {
    const cwd = await parseTaskFixture(
      "schema-payload",
      `import { smokeSandbox } from "../fixture/sandbox"
import { task, type PayloadSchema } from "@helmr/sdk"

const payloadSchema: PayloadSchema<{ readonly issue: string }, { readonly issue: number }> = {
  "~standard": {
    version: 1,
    vendor: "test",
    validate(value) {
      if (value === null || typeof value !== "object") {
        return { issues: [{ message: "expected object" }] }
      }
      const issue = (value as Record<string, unknown>)["issue"]
      if (typeof issue !== "string") {
        return { issues: [{ message: "expected string", path: ["issue"] }] }
      }
      return { value: { issue: Number(issue) } }
    },
  },
  toJSONSchema() {
    return {}
  },
}

export const schemaPayload = task({
  id: "schema-payload",
  sandbox: smokeSandbox,
  payloadSchema,
  run: async (payload) => ({ issue: payload.issue + 1 }),
})
`,
    )
    const passed = await runAdapterTask(cwd, "schema-payload", {
      payloadJson: JSON.stringify({ issue: "41" }),
    })
    expect(passed.status, passed.stderr).toBe(0)
    expect(taskOutput(passed)).toEqual({ issue: 42 })

    const failed = await runAdapterTask(cwd, "schema-payload", {
      payloadJson: JSON.stringify({ issue: 41 }),
    })
    expect(failed.status).toBe(0)
    expect(taskExitCode(failed)).toBe(1)
    expect(taskErrorMessage(failed)).toContain("payload.issue: expected string")
  })

  test("adapter run passes task context metadata into task context", async () => {
    const cwd = await taskFixture(
      "context",
      `({
        runId: ctx.run.id,
        taskId: ctx.task.id,
        sourceKind: ctx.source.kind,
        repository: ctx.source.repository,
        requestedRef: ctx.source.requestedRef,
        resolvedSha: ctx.source.resolvedSha,
        refKind: ctx.source.refKind,
        pullRequestBaseRef: ctx.source.pullRequest?.baseRef,
        workspacePath: ctx.workspace.path,
        projectPath: ctx.workspace.projectPath,
      })`,
    )
    const result = await runAdapterTask(cwd, "context", {
      runId: "run-context",
      taskContextJson: sampleTaskContextJSON({
        runId: "run-context",
        taskId: "context",
        refKind: "pull_request",
        pullRequest: {
          number: 42,
          baseRef: "main",
          baseSha: "0123456789abcdef0123456789abcdef01234567",
          headRef: "feature",
          headSha: "0123456789abcdef0123456789abcdef01234568",
        },
      }),
    })

    expect(result.status, result.stderr).toBe(0)
    expect(taskOutput(result)).toEqual({
      runId: "run-context",
      taskId: "context",
      sourceKind: "github",
      repository: "helmrdotdev/helmr",
      requestedRef: "main",
      resolvedSha: "0123456789abcdef0123456789abcdef01234567",
      refKind: "pull_request",
      pullRequestBaseRef: "main",
      workspacePath: "/workspace",
      projectPath: "/workspace",
    })
  })

  test("adapter run rejects task context run id mismatch", async () => {
    const cwd = await taskFixture("context-mismatch", "({ ok: true })")
    const result = await runAdapterTask(cwd, "context-mismatch", {
      runId: "run-a",
      taskContextJson: sampleTaskContextJSON({ runId: "run-b", taskId: "context-mismatch" }),
    })

    expect(result.status).toBe(0)
    expect(taskExitCode(result)).toBe(1)
    expect(taskErrorMessage(result)).toContain('task context run.id "run-b" does not match --run-id "run-a"')
    const error = JSON.parse(result.stderr.trim())
    expect(error.message).toContain('task context run.id "run-b" does not match --run-id "run-a"')
  })

  test("adapter run discovers tasks by id instead of filename", async () => {
    const cwd = await mkdtemp(resolve(tmpdir(), "helmr-adapter-module-test-"))
    await mkdir(resolve(cwd, "tasks/custom"), { recursive: true })
    await writeFile(
      resolve(cwd, "tasks/custom/review.ts"),
      'import { image, sandbox, task } from "@helmr/sdk"\nconst sb = sandbox("hello").image(image("hello").from("debian:trixie-slim")).workspace("/app")\nexport const review = task({ id: "hello", sandbox: sb, run: async () => ({ module: "custom" }) })\n',
    )
    await writeFile(
      resolve(cwd, "helmr.config.ts"),
      'import { defineConfig } from "@helmr/sdk"\nexport default defineConfig({ project: "local-deploys", dirs: ["./tasks"] })\n',
    )
    const result = await runAdapterTask(cwd, "hello")

    expect(result.status, result.stderr).toBe(0)
    expect(taskOutput(result)).toEqual({ module: "custom" })
  })

  test("adapter run reports a fuzzy suggestion when task id is missing", async () => {
    const cwd = await taskFixture("codex-review", "({ ok: true })", "", "review.ts")
    const result = await runAdapterTask(cwd, "codex-reveiw")

    expect(result.status).toBe(0)
    expect(taskExitCode(result)).toBe(1)
    const error = JSON.parse(result.stderr.trim())
    expect(error).toMatchObject({
      level: "error",
      kind: "task_not_found",
      message: 'task "codex-reveiw" not found (did you mean "codex-review"?)\navailable: codex-review',
    })
  })

  test("adapter run does not suggest unrelated task ids", async () => {
    const cwd = await taskFixture("bar", "({ ok: true })", "", "foo")
    const result = await runAdapterTask(cwd, "foo")

    expect(result.status).toBe(0)
    expect(taskExitCode(result)).toBe(1)
    const error = JSON.parse(result.stderr.trim())
    expect(error).toMatchObject({
      level: "error",
      kind: "task_not_found",
      message: 'task "foo" not found\navailable: bar',
    })
  })

  test("adapter parse rejects default-exported task refs", async () => {
    const cwd = await parseTaskFixture(
      "default-task",
      `
import { image, sandbox, task } from "@helmr/sdk"

const sb = sandbox("default-task")
  .image(image("default-task").from("debian:trixie-slim"))
  .workspace("/app")

export default task({
  id: "default-task",
  sandbox: sb,
  run: async () => ({ ok: true }),
})
`,
    )
    const result = await parseAdapterTask(cwd, "default-task")

    expect(result.status).toBe(1)
    const error = JSON.parse(result.stderr.trim())
    expect(error).toMatchObject({
      level: "error",
      kind: "bad_request",
    })
    expect(error.message).toContain('default-exports task "default-task"')
    expect(error.message).toContain("use a named export")
  })

  test("adapter parse emits duplicate task id kind", async () => {
    const cwd = await mkdtemp(resolve(tmpdir(), "helmr-adapter-duplicate-kind-test-"))
    await mkdir(resolve(cwd, "tasks/agents"), { recursive: true })
    const source = (filename: string) =>
      `import { image, sandbox, task } from "@helmr/sdk"\nconst sb = sandbox("dup").image(image("dup").from("debian:trixie-slim")).workspace("/app")\nexport const dupTask = task({ id: "dup", sandbox: sb, run: async () => ({ file: ${JSON.stringify(filename)} }) })\n`
    await writeFile(resolve(cwd, "tasks/dup.ts"), source("dup.ts"))
    await writeFile(resolve(cwd, "tasks/agents/dup.ts"), source("agents/dup.ts"))
    await writeFile(
      resolve(cwd, "helmr.config.ts"),
      'import { defineConfig } from "@helmr/sdk"\nexport default defineConfig({ project: "local-deploys", dirs: ["./tasks"] })\n',
    )

    const result = await parseAdapterTask(cwd, "dup")

    expect(result.status).toBe(1)
    const error = JSON.parse(result.stderr.trim())
    expect(error).toMatchObject({
      level: "error",
      kind: "duplicate_task_id",
    })
    expect(error.message).toContain('duplicate task id "dup"')
    expect(error.message).toContain("tasks/dup.ts")
    expect(error.message).toContain("tasks/agents/dup.ts")
  })

  test("adapter parse discovers .mts task files", async () => {
    const cwd = await taskFixtureWithExtension("mts-task", "({ ok: true })", ".mts")
    const result = await parseAdapterTask(cwd, "mts-task")

    expect(result.status, result.stderr).toBe(0)
    const decoded = fromBinary(BundleSchema, new Uint8Array(result.stdout))
    expect(decoded.task?.modulePath).toBe("tasks/mts-task.mts")
  })

  test("adapter run emits token requests and fails on closed response stream", async () => {
    const cwd = await taskFixture(
      "needs-token",
      "ctx.wait.token({ displayText: 'ship it', policy: 'prod-deploy-approval' })",
    )
    const result = await runAdapterTask(cwd, "needs-token")

    expect(result.status).toBe(0)
    expect(taskExitCode(result)).toBe(1)
    expect(result.controlEvents[0]?.event).toMatchObject({
      case: "waitRequested",
      value: {
        correlationId: "1",
        kind: "token",
        requestJson: JSON.stringify({}),
        displayText: "ship it",
        policy: "prod-deploy-approval",
      },
    })
    const error = JSON.parse(result.stderr.trim())
    expect(error).toMatchObject({
      level: "error",
      message: "adapter response stream closed",
    })
  })

  test("adapter run surfaces token timeout errors from host-driven timeout responses", async () => {
    const cwd = await taskFixture(
      "token-timeout",
      "(async () => { try { await ctx.wait.token({ displayText: 'ship it', timeout: 1 }); return { ok: false } } catch (error) { return { name: error instanceof Error ? error.name : String(error), message: error instanceof Error ? error.message : String(error) } } })()",
    )
    const result = await runAdapterTaskInteractively(
      cwd,
      "token-timeout",
      async ({ stdin, waitForControlEvent }) => {
        await waitForControlEvent("waitRequested")
        stdin.write(resumeDecisionFrame({
          waitpointId: "waitpoint-1",
          kind: "timed_out",
          resolutionPayloadJson: JSON.stringify({ at: "2026-04-23T00:00:00Z" }),
          timedOut: true,
        }))
        stdin.end()
      },
    )

    expect(result.status, result.stderr).toBe(0)
    expect(taskOutput(result)).toMatchObject({
      name: "Error",
      message: "token wait timed out after 1",
    })
  })

  test("adapter run emits generic wait.for requests", async () => {
    const cwd = await taskFixture(
      "needs-wait-for",
      "ctx.wait.for({ seconds: 10 }, { displayText: 'ten seconds' })",
    )
    const result = await runAdapterTask(cwd, "needs-wait-for")

    expect(result.status).toBe(0)
    expect(taskExitCode(result)).toBe(1)
    const event = result.controlEvents[0]?.event
    expect(event).toMatchObject({
      case: "waitRequested",
      value: {
        correlationId: "1",
        kind: "delay",
        requestJson: JSON.stringify({ seconds: 10 }),
        displayText: "ten seconds",
        timeout: 10,
      },
    })
    if (event?.case !== "waitRequested") throw new Error("expected waitRequested")
  })

  test("adapter run rounds millisecond wait.for requests up to seconds", async () => {
    const cwd = await taskFixture("needs-wait-for-ms", "ctx.wait.for({ milliseconds: 1500 })")
    const result = await runAdapterTask(cwd, "needs-wait-for-ms")

    expect(result.status).toBe(0)
    expect(taskExitCode(result)).toBe(1)
    expect(result.controlEvents[0]?.event).toMatchObject({
      case: "waitRequested",
      value: {
        correlationId: "1",
        kind: "delay",
        requestJson: JSON.stringify({ milliseconds: 1500 }),
        timeout: 2,
      },
    })
  })

  test("adapter run accepts duration wait.for requests", async () => {
    const cwd = await taskFixture("needs-wait-for-duration", "ctx.wait.for('1.5s')")
    const result = await runAdapterTask(cwd, "needs-wait-for-duration")

    expect(result.status).toBe(0)
    expect(taskExitCode(result)).toBe(1)
    expect(result.controlEvents[0]?.event).toMatchObject({
      case: "waitRequested",
      value: {
        correlationId: "1",
        kind: "delay",
        requestJson: JSON.stringify({ duration: "1.5s" }),
        timeout: 2,
      },
    })
  })

  test("adapter run emits generic wait.until requests", async () => {
    const cwd = await taskFixture(
      "needs-wait-until",
      "ctx.wait.until(new Date('2026-04-23T00:00:00Z'), { displayText: 'deadline' })",
    )
    const result = await runAdapterTask(cwd, "needs-wait-until")

    expect(result.status).toBe(0)
    expect(taskExitCode(result)).toBe(1)
    expect(result.controlEvents[0]?.event).toMatchObject({
      case: "waitRequested",
      value: {
        correlationId: "1",
        kind: "delay",
        requestJson: JSON.stringify({ date: "2026-04-23T00:00:00.000Z" }),
        displayText: "deadline",
        timeout: 1,
      },
    })
  })

  test("adapter run resolves generic wait.token completions", async () => {
    const cwd = await taskFixture("wait-token", "ctx.wait.token()")
    const result = await runAdapterTaskInteractively(
      cwd,
      "wait-token",
      async ({ stdin, waitForControlEvent }) => {
        await waitForControlEvent("waitRequested")
        stdin.write(resumeDecisionFrame({
          waitpointId: "waitpoint-1",
          kind: "completed",
          resolutionPayloadJson: JSON.stringify({
            value: { ok: true },
            at: "2026-04-23T00:00:00Z",
          }),
        }))
        stdin.end()
      },
    )

    expect(result.status, result.stderr).toBe(0)
    expect(result.controlEvents[0]?.event).toMatchObject({
      case: "waitRequested",
      value: {
        correlationId: "1",
        kind: "token",
        requestJson: JSON.stringify({}),
      },
    })
    expect(taskOutput(result)).toEqual({ ok: true })
  })

  test("adapter run parses wait.token completions with validation-only schemas", async () => {
    const cwd = await taskFixture(
      "wait-token-validation-schema",
      `(async () => {
        const schema: PayloadValidationSchema<unknown, { readonly approved: boolean }> = {
          "~standard": {
            version: 1,
            vendor: "test",
            validate(value) {
              if (value === null || typeof value !== "object") {
                return { issues: [{ message: "expected object" }] }
              }
              const approved = (value as Record<string, unknown>)["approved"]
              if (typeof approved !== "boolean") {
                return { issues: [{ message: "expected boolean", path: ["approved"] }] }
              }
              return { value: { approved } }
            },
          },
        }
        return await ctx.wait.token({ schema })
      })()`,
      `import type { PayloadValidationSchema } from "@helmr/sdk"\n`,
    )
    const result = await runAdapterTaskInteractively(
      cwd,
      "wait-token-validation-schema",
      async ({ stdin, waitForControlEvent }) => {
        await waitForControlEvent("waitRequested")
        stdin.write(resumeDecisionFrame({
          waitpointId: "waitpoint-1",
          kind: "completed",
          resolutionPayloadJson: JSON.stringify({
            value: { approved: true },
            at: "2026-04-23T00:00:00Z",
          }),
        }))
        stdin.end()
      },
    )

    expect(result.status, result.stderr).toBe(0)
    expect(taskOutput(result)).toEqual({ approved: true })
  })

  test("adapter run rejects token resume payloads with missing at", async () => {
    const cwd = await taskFixture(
      "token-missing-at",
      "(async () => { try { return await ctx.wait.token<{ ok: boolean }>() } catch (error) { return { message: error instanceof Error ? error.message : String(error) } } })()",
    )
    const result = await runAdapterTaskInteractively(
      cwd,
      "token-missing-at",
      async ({ stdin, waitForControlEvent }) => {
        await waitForControlEvent("waitRequested")
        stdin.write(resumeDecisionFrame({
          waitpointId: "waitpoint-1",
          kind: "completed",
          resolutionPayloadJson: JSON.stringify({ value: { ok: true } }),
        }))
        stdin.end()
      },
    )

    expect(result.status, result.stderr).toBe(0)
    expect(taskOutput(result)).toEqual({
      message: "resume payload at is required and must be a valid timestamp",
    })
  })

  test("adapter run rejects token resume payloads with invalid at", async () => {
    const cwd = await taskFixture(
      "token-invalid-at",
      "(async () => { try { return await ctx.wait.token<{ ok: boolean }>() } catch (error) { return { message: error instanceof Error ? error.message : String(error) } } })()",
    )
    const result = await runAdapterTaskInteractively(
      cwd,
      "token-invalid-at",
      async ({ stdin, waitForControlEvent }) => {
        await waitForControlEvent("waitRequested")
        stdin.write(resumeDecisionFrame({
          waitpointId: "waitpoint-1",
          kind: "completed",
          resolutionPayloadJson: JSON.stringify({ value: { ok: true }, at: "not-a-date" }),
        }))
        stdin.end()
      },
    )

    expect(result.status, result.stderr).toBe(0)
    expect(taskOutput(result)).toEqual({
      message: "resume payload at is required and must be a valid timestamp",
    })
  })

  test("adapter run rejects resume decisions with the wrong kind for token waits", async () => {
    const cwd = await taskFixture("token-wrong-kind", "ctx.wait.token()")
    const result = await runAdapterTaskInteractively(
      cwd,
      "token-wrong-kind",
      async ({ stdin, waitForControlEvent }) => {
        await waitForControlEvent("waitRequested")
        stdin.write(resumeDecisionFrame({
          waitpointId: "waitpoint-1",
          kind: "unexpected",
          resolutionPayloadJson: JSON.stringify({ principal: "alice" }),
        }))
        stdin.end()
      },
    )

    expect(result.status).toBe(0)
    expect(taskExitCode(result)).toBe(1)
    expect(JSON.parse(result.stderr.trim()).message).toBe('unexpected token resume decision kind "unexpected"')
  })

  test("adapter run rejects concurrent waits with ConcurrentWaitError", async () => {
    const cwd = await taskFixture(
      "concurrent-wait",
      "(async () => { const first = ctx.wait.token({ displayText: 'one' }).catch(() => undefined); try { await ctx.wait.token({ displayText: 'two' }); return { ok: false } } catch (error) { return { concurrent: error instanceof ConcurrentWaitError, name: error instanceof Error ? error.name : String(error), message: error instanceof Error ? error.message : String(error) } } finally { await first } })()",
      "import { ConcurrentWaitError } from \"@helmr/sdk\"\n",
    )
    const result = await runAdapterTask(cwd, "concurrent-wait")

    expect(result.status, result.stderr).toBe(0)
    expect(result.controlEvents[0]?.event.case).toBe("waitRequested")
    expect(taskOutput(result)).toMatchObject({
      concurrent: true,
      name: "ConcurrentWaitError",
      message: "concurrent ctx.wait.* calls are not supported in v0.1",
    })
  })

  test("adapter run rejects oversized wait display text before emitting control events", async () => {
    const cwd = await taskFixture(
      "oversized-wait",
      `ctx.wait.token({ displayText: ${JSON.stringify("x".repeat(16 * 1024 + 1))} })`,
    )
    const result = await runAdapterTask(cwd, "oversized-wait")

    expect(result.status).toBe(0)
    expect(taskExitCode(result)).toBe(1)
    expect(result.controlEvents.map((event) => event.event.case)).not.toContain("waitRequested")
    const error = JSON.parse(result.stderr.trim())
    expect(error.message).toContain("wait display text")
    expect(error.message).toContain("exceeds max 16384")
  })

  test("adapter run emits MCP-shaped events", async () => {
    const cwd = await taskFixture(
      "emits",
      "(() => { ctx.emit({ type: 'agent.event', content: [{ type: 'text', text: 'hi' }] }); return { ok: true } })()",
    )
    const result = await runAdapterTask(cwd, "emits")

    expect(result.status, result.stderr).toBe(0)
    expect(result.controlEvents[0]?.event).toMatchObject({
      case: "emitEvent",
      value: {
        type: "agent.event",
        contentJson: JSON.stringify([{ type: "text", text: "hi" }]),
      },
    })
  })

  test("adapter run rejects oversized emit events before emitting control events", async () => {
    const cwd = await taskFixture(
      "oversized-emit",
      "(() => { ctx.emit({ type: 'agent.event', content: ['" + "x".repeat(256 * 1024) + "'] }); return { ok: true } })()",
    )
    const result = await runAdapterTask(cwd, "oversized-emit")

    expect(result.status).toBe(0)
    expect(taskExitCode(result)).toBe(1)
    expect(result.controlEvents.map((event) => event.event.case)).not.toContain("emitEvent")
    const error = JSON.parse(result.stderr.trim())
    expect(error.message).toContain("emit event content_json")
    expect(error.message).toContain("exceeds max 262144")
  })

  test("adapter run truncates oversized ctx.log entries", async () => {
    const cwd = await taskFixture(
      "oversized-log",
      "(() => { ctx.log.info('" + "x".repeat(70 * 1024) + "'); return { ok: true } })()",
    )
    const result = await runAdapterTask(cwd, "oversized-log")

    expect(result.status, result.stderr).toBe(0)
    expect(result.controlEvents[0]?.event.case).toBe("logEntry")
    const entry = result.controlEvents[0]?.event.value
    expect(typeof entry).toBe("string")
    expect(Buffer.byteLength(entry as string, "utf8")).toBeLessThanOrEqual(64 * 1024)
    const payload = JSON.parse(entry as string)
    expect(payload.message).toContain("...[truncated ctx.log entry]")
  })

  test("adapter run propagates task throws as error JSON", async () => {
    const cwd = await taskFixture("throws", "Promise.reject(new Error('task exploded'))")
    const result = await runAdapterTask(cwd, "throws")

    expect(result.status).toBe(0)
    expect(taskExitCode(result)).toBe(1)
    const error = JSON.parse(result.stderr.trim())
    expect(error).toMatchObject({ level: "error", message: "task exploded" })
  })

  test("adapter registry import surfaces top-level dependency failures", async () => {
    const cwd = await taskFixture(
      "missing-runtime-import",
      "query({ prompt: 'run should resolve real packages' })",
      'import { query } from "missing-agent-sdk"\n',
    )
    const result = await runAdapterTask(cwd, "missing-runtime-import")

    expect(result.status).toBe(0)
    expect(taskExitCode(result)).toBe(1)
    const error = JSON.parse(result.stderr.trim())
    expect(error.message).toContain("Cannot find package 'missing-agent-sdk'")
    expect(error.message).toContain("missing-runtime-import.ts")
  })
})

function compileFixtureTask() {
  const deps = source.directory(".", {
    ignore: ["*", "!package.json", "!bun.lock", "!tsconfig.json"],
  })
  const base = image("compile-fixture")
    .from("debian:trixie-slim")
    .run(["apt-get", "update"])
    .copy("/app", deps)
    .workdir("/app")
    .env("PYTHONUNBUFFERED", "1")
    .user("agent")
  const smokeSandbox = sandbox("compile-fixture")
    .image(base)
    .workspace("/app")
    .resources({ cpu: 2, memory: "4Gi" })

  return task({
      id: "hello",
      sandbox: smokeSandbox,
      maxDuration: 5 * 60,
      secrets: {
        githubToken: { env: "GITHUB_TOKEN" },
      },
      run: async () => ({ ok: true }),
  })
}

async function parseTaskFixture(taskId: string, taskSource: string): Promise<string> {
  const cwd = await mkdtemp(resolve(tmpdir(), "helmr-adapter-parse-test-"))
  await mkdir(resolve(cwd, "tasks"))
  const sandboxSource = `import { image, sandbox } from "@helmr/sdk"

const base = image(${JSON.stringify(taskId)}).from("debian:trixie-slim")

const smokeSandbox = sandbox(${JSON.stringify(taskId)})
  .image(base)
  .workspace("/app")
`
  await writeFile(
    resolve(cwd, "tasks", `${taskId}.ts`),
    taskSource.replace('import { smokeSandbox } from "../fixture/sandbox"', sandboxSource),
  )
  await writeFile(
    resolve(cwd, "helmr.config.ts"),
    'import { defineConfig } from "@helmr/sdk"\nexport default defineConfig({ project: "local-deploys", dirs: ["./tasks"] })\n',
  )
  return cwd
}

async function taskFixture(
  taskId: string,
  expression: string,
  imports = "",
  filename = taskId,
): Promise<string> {
  const cwd = await mkdtemp(resolve(tmpdir(), "helmr-adapter-test-"))
  await mkdir(resolve(cwd, "tasks"))
  await writeFile(
    resolve(cwd, "tasks", `${filename}.ts`),
    `${imports}import { image, sandbox, task } from "@helmr/sdk"\nconst sb = sandbox(${JSON.stringify(taskId)}).image(image(${JSON.stringify(taskId)}).from("debian:trixie-slim")).workspace("/app")\nexport const discoveredTask = task({ id: ${JSON.stringify(taskId)}, sandbox: sb, run: async (ctx) => ${expression} })\n`,
  )
  await writeFile(
    resolve(cwd, "helmr.config.ts"),
    'import { defineConfig } from "@helmr/sdk"\nexport default defineConfig({ project: "local-deploys", dirs: ["./tasks"] })\n',
  )
  return cwd
}

async function taskFixtureWithExtension(
  taskId: string,
  expression: string,
  extension: ".mts" | ".cts",
): Promise<string> {
  const cwd = await mkdtemp(resolve(tmpdir(), `helmr-adapter-${extension.slice(1)}-test-`))
  await mkdir(resolve(cwd, "tasks"))
  await writeFile(
    resolve(cwd, "tasks", `${taskId}${extension}`),
    `import { image, sandbox, task } from "@helmr/sdk"\nconst sb = sandbox(${JSON.stringify(taskId)}).image(image(${JSON.stringify(taskId)}).from("debian:trixie-slim")).workspace("/app")\nexport const discoveredTask = task({ id: ${JSON.stringify(taskId)}, sandbox: sb, run: async (ctx) => ${expression} })\n`,
  )
  await writeFile(
    resolve(cwd, "helmr.config.ts"),
    'import { defineConfig } from "@helmr/sdk"\nexport default defineConfig({ project: "local-deploys", dirs: ["./tasks"] })\n',
  )
  return cwd
}

async function parseAdapterTask(
  cwd: string,
  taskId: string,
): Promise<{ readonly stdout: Uint8Array; readonly stderr: string; readonly status: number }> {
  return invokeAdapter(["parse", "--cwd", cwd, "--task", taskId, "--output", "binary"])
}

async function runAdapterTask(
  cwd: string,
  taskId: string,
  options: RunAdapterTaskOptions = {},
): Promise<{
  readonly stdout: string
  readonly stderr: string
  readonly status: number
  readonly controlEvents: readonly runProto.RunEvent[]
}> {
  const result = await invokeAdapter(adapterRunArgs(cwd, taskId, options))
  return {
    stdout: result.stdout.toString(),
    stderr: result.stderr,
    status: result.status,
    controlEvents: decodeRunEvents(result.control),
  }
}

async function runAdapterTaskInteractively(
  cwd: string,
  taskId: string,
  interact: (helpers: {
    readonly stdin: PassThrough
    readonly waitForControlEvent: (kind: runProto.RunEvent["event"]["case"]) => Promise<void>
  }) => Promise<void>,
  options: RunAdapterTaskOptions = {},
): Promise<{
  readonly stdout: string
  readonly stderr: string
  readonly status: number
  readonly controlEvents: readonly runProto.RunEvent[]
}> {
  const sdkRoot = fileURLToPath(new URL("..", import.meta.url))
  await linkLocalSdk(resolve(options.taskCwd ?? cwd), sdkRoot)
  const stdin = new PassThrough()
  const stdout = new CaptureSink()
  const stderr = new CaptureSink()
  const control = new CaptureSink()
  const io: AdapterIo = { stdin, stdout, stderr, control }
  const resultPromise = runAdapterCli(adapterRunArgs(cwd, taskId, options), io)
  const waitForControlEvent = async (kind: runProto.RunEvent["event"]["case"]): Promise<void> => {
    for (let attempt = 0; attempt < 100; attempt += 1) {
      if (decodeRunEvents(control.bytes()).some((event) => event.event.case === kind)) {
        return
      }
      await new Promise((resolve) => setTimeout(resolve, 5))
    }
    throw new Error(`timed out waiting for control event: ${kind}`)
  }

  await interact({ stdin, waitForControlEvent })
  const status = await resultPromise
  return {
    stdout: stdout.bytes().toString(),
    stderr: stderr.text(),
    status,
    controlEvents: decodeRunEvents(control.bytes()),
  }
}

interface RunAdapterTaskOptions {
  readonly runId?: string
  readonly payloadJson?: string
  readonly taskCwd?: string
  readonly taskContextJson?: string
}

function sampleTaskContextJSON(options: {
  readonly runId: string
  readonly taskId: string
  readonly refKind?: string
  readonly pullRequest?: {
    readonly number: number
    readonly baseRef: string
    readonly baseSha: string
    readonly headRef: string
    readonly headSha: string
  }
}): string {
  return JSON.stringify({
    run: { id: options.runId },
    task: { id: options.taskId },
    source: {
      kind: "github",
      repository: "helmrdotdev/helmr",
      requestedRef: "main",
      resolvedSha: "0123456789abcdef0123456789abcdef01234567",
      refKind: options.refKind ?? "branch",
      refName: "main",
      fullRef: "refs/heads/main",
      defaultBranch: "main",
      ...(options.pullRequest === undefined ? {} : { pullRequest: options.pullRequest }),
    },
    workspace: {
      path: "/workspace",
      projectPath: "/workspace",
    },
  })
}

function adapterRunArgs(
  cwd: string,
  taskId: string,
  options: RunAdapterTaskOptions = {},
): string[] {
  const argv = [
    "run",
    "--cwd",
    cwd,
    "--task",
    taskId,
    "--run-id",
    options.runId ?? "run-1",
    "--task-context-json",
    options.taskContextJson ?? sampleTaskContextJSON({ runId: options.runId ?? "run-1", taskId }),
  ]
  if (options.payloadJson !== undefined) {
    argv.push("--payload-json", options.payloadJson)
  }
  if (options.taskCwd !== undefined) {
    argv.push("--task-cwd", options.taskCwd)
  }
  return argv
}

async function invokeAdapter(
  argv: readonly string[],
  stdin: NodeJS.ReadableStream = Readable.from([]),
): Promise<{
  readonly stdout: Buffer
  readonly stderr: string
  readonly status: number
  readonly control: Buffer
}> {
  const sdkRoot = fileURLToPath(new URL("..", import.meta.url))
  const taskCwd = optionValue(argv, "--task-cwd") ?? optionValue(argv, "--cwd")
  if (taskCwd !== undefined) {
    await linkLocalSdk(resolve(taskCwd), sdkRoot)
  }
  const stdout = new CaptureSink()
  const stderr = new CaptureSink()
  const control = new CaptureSink()
  const io: AdapterIo = {
    stdin,
    stdout,
    stderr,
    control,
  }
  const status = await runAdapterCli(argv, io)
  return { stdout: stdout.bytes(), stderr: stderr.text(), status, control: control.bytes() }
}

function optionValue(argv: readonly string[], name: string): string | undefined {
  const index = argv.indexOf(name)
  return index === -1 ? undefined : argv[index + 1]
}

async function linkLocalSdk(cwd: string, sdkRoot: string): Promise<void> {
  const packagePath = resolve(cwd, "package.json")
  try {
    await realpath(packagePath)
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

function resumeDecisionFrame(decision: {
  readonly waitpointId?: string
  readonly kind?: string
  readonly resolutionPayloadJson?: string
  readonly timedOut?: boolean
}): Buffer {
  const body = Buffer.from(toBinary(
    runProto.ResumeDecisionSchema,
    create(runProto.ResumeDecisionSchema, decision),
  ))
  return frame(body)
}

function decodeRunEvents(bytes: Uint8Array): runProto.RunEvent[] {
  const buffer = Buffer.from(bytes)
  const events: runProto.RunEvent[] = []
  let cursor = 0
  while (cursor + 4 <= buffer.length) {
    const length = buffer.readUInt32BE(cursor)
    cursor += 4
    if (cursor + length > buffer.length) {
      break
    }
    const body = buffer.subarray(cursor, cursor + length)
    cursor += length
    events.push(fromBinary(runProto.RunEventSchema, body))
  }
  return events
}

function taskOutput(result: { readonly controlEvents: readonly runProto.RunEvent[] }): unknown {
  const outcome = taskOutcome(result)
  if (outcome.outputJson === undefined) {
    throw new Error("missing task output event")
  }
  return JSON.parse(outcome.outputJson)
}

function taskExitCode(result: { readonly controlEvents: readonly runProto.RunEvent[] }): number {
  return taskOutcome(result).exitCode
}

function taskErrorMessage(result: { readonly controlEvents: readonly runProto.RunEvent[] }): string {
  const message = taskOutcome(result).errorMessage
  if (message === undefined) {
    throw new Error("missing task outcome error message")
  }
  return message
}

function taskOutcome(result: { readonly controlEvents: readonly runProto.RunEvent[] }): runProto.TaskOutcome {
  const event = result.controlEvents.find((event) => event.event.case === "taskOutcome")
  if (event?.event.case !== "taskOutcome") {
    throw new Error("missing task outcome event")
  }
  return event.event.value
}

function frame(body: Uint8Array): Buffer {
  const header = Buffer.alloc(4)
  header.writeUInt32BE(body.length, 0)
  return Buffer.concat([header, Buffer.from(body)])
}

class CaptureSink {
  readonly #chunks: Buffer[] = []

  write(chunk: string | Uint8Array): boolean {
    this.#chunks.push(typeof chunk === "string" ? Buffer.from(chunk) : Buffer.from(chunk))
    return true
  }

  bytes(): Buffer {
    return Buffer.concat(this.#chunks)
  }

  text(): string {
    return this.bytes().toString()
  }
}

function expectPresent<T>(value: T | undefined, label: string): T {
  expect(value, `${label} must be present`).toBeDefined()
  if (value === undefined) {
    throw new Error(`${label} must be present`)
  }
  return value
}

const IMAGE_KEY_DOMAIN = new TextEncoder().encode("helmr.image.v0\n")

function tsImageKey(image: ImageSpec): string {
  const hash = createHash("sha256")
  hash.update(IMAGE_KEY_DOMAIN)
  hash.update(u32be(image.formatVersion))
  hashLenPrefixedBytes(
    hash,
    image.platform ? toBinary(PlatformSchema, image.platform) : new Uint8Array(),
  )
  hashLenPrefixedBytes(hash, encodeImageSteps(image.steps))
  hashLenPrefixedBytes(hash, encodeDigestList(sourceInputDigests(image.steps)))
  hashLenPrefixedBytes(hash, encodeDigestList(subImageKeys(image.steps)))
  return `sha256:${hash.digest("hex")}`
}

function encodeImageSteps(steps: readonly ImageStep[]): Uint8Array {
  const chunks = [u64be(steps.length)]
  for (const step of steps) {
    chunks.push(lenPrefixedBytes(toBinary(ImageStepSchema, step)))
  }
  return concatBytes(chunks)
}

function encodeDigestList(values: readonly string[]): Uint8Array {
  const chunks = [u64be(values.length)]
  for (const value of values) {
    chunks.push(lenPrefixedBytes(new TextEncoder().encode(value)))
  }
  return concatBytes(chunks)
}

function sourceInputDigests(steps: readonly ImageStep[]): string[] {
  const values: string[] = []
  for (const step of steps) {
    if (step.kind.case === "copySourceFile") {
      values.push(step.kind.value.digest)
    }
    if (step.kind.case === "copySourceDir") {
      values.push(step.kind.value.treeDigest)
    }
  }
  return values
}

function subImageKeys(steps: readonly ImageStep[]): string[] {
  const values: string[] = []
  for (const step of steps) {
    if (step.kind.case === "copyFromImage") {
      values.push(step.kind.value.srcImageKey)
    }
  }
  return values
}

function hashLenPrefixedBytes(hash: ReturnType<typeof createHash>, bytes: Uint8Array): void {
  hash.update(u64be(bytes.byteLength))
  hash.update(bytes)
}

function lenPrefixedBytes(bytes: Uint8Array): Uint8Array {
  return concatBytes([u64be(bytes.byteLength), bytes])
}

function u32be(value: number): Uint8Array {
  const buffer = Buffer.alloc(4)
  buffer.writeUInt32BE(value)
  return buffer
}

function u64be(value: number): Uint8Array {
  const buffer = Buffer.alloc(8)
  buffer.writeBigUInt64BE(BigInt(value))
  return buffer
}

function concatBytes(chunks: readonly Uint8Array[]): Uint8Array {
  const total = chunks.reduce((sum, chunk) => sum + chunk.byteLength, 0)
  const out = new Uint8Array(total)
  let offset = 0
  for (const chunk of chunks) {
    out.set(chunk, offset)
    offset += chunk.byteLength
  }
  return out
}
