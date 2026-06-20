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
import { validateSecretName } from "./internal"
import { image, queue, sandbox, schedules, source, task, type PayloadSchema } from "./index"

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
    expect(sandbox.resources?.disk).toBe("32Gi")
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

  test("task schedule is emitted in the bundle", () => {
    const bundle = compile({
      task: schedules.task({
        id: "scheduled",
        sandbox: sandbox("scheduled").image(image("scheduled").from("debian:trixie-slim")),
        cron: {
          pattern: "0 2 * * *",
          timezone: "Asia/Tokyo",
        },
        run: async () => null,
      }),
      modulePath: "tasks/scheduled.ts",
    })

    expect(bundle.task?.schedules).toEqual([
      {
        $typeName: "helmr.bundle.v0.TaskScheduleSpec",
        id: "",
        cron: "0 2 * * *",
        timezone: "Asia/Tokyo",
      },
    ])
  })

  test("emits task queue and ttl metadata", () => {
    const serialQueue = queue({ name: "review/pr", concurrencyLimit: 1 })
    const bundle = compile({
      task: task({
        id: "queued-task",
        sandbox: sandbox("queued-task").image(image("queued-task").from("debian:trixie-slim")),
        queue: serialQueue,
        ttl: "10m",
        run: async () => null,
      }),
      modulePath: "tasks/queued-task.ts",
    })
    expect(bundle.task?.queue?.name).toBe("review/pr")
    expect(bundle.task?.queue?.concurrencyLimit).toBe(1)
    expect(bundle.task?.ttl).toBe("10m")
  })

  test("emits task retry policy metadata", () => {
    const bundle = compile({
      task: task({
        id: "retrying-task",
        sandbox: sandbox("retrying-task").image(image("retrying-task").from("debian:trixie-slim")),
        retry: { maxAttempts: 3, backoff: { minMs: 1000, maxMs: 30000, factor: 2, jitter: "full" } },
        run: async () => null,
      }),
      modulePath: "tasks/retrying-task.ts",
    })

    expect(bundle.task?.retryPolicyJson).toBe(JSON.stringify({
      maxAttempts: 3,
      backoff: { minMs: 1000, maxMs: 30000, factor: 2, jitter: "full" },
    }))
  })

  test("default queue preserves dotted task ids", () => {
    const bundle = compile({
      task: task({
        id: "build.test",
        sandbox: sandbox("build-test").image(image("build-test").from("debian:trixie-slim")),
        run: async () => null,
      }),
      modulePath: "tasks/build.test.ts",
    })
    expect(bundle.task?.queue?.name).toBe("task/build.test")
  })

  test("does not emit payload schema metadata", () => {
    const payload: PayloadSchema<unknown> = {
      "~standard": {
        version: 1,
        vendor: "test",
        validate(value) {
          return { value }
        },
      },
    }
    const bundle = compile({
      task: task({
        id: "schema-metadata",
        sandbox: sandbox("schema-metadata").image(image("schema-metadata").from("debian:trixie-slim")),
        payload,
        run: async (payload) => payload,
      }),
      modulePath: "tasks/schema-metadata.ts",
    })

    expect(Object.keys(bundle.task ?? {})).not.toContain("payloadSchemaJson")
  })

  test("secret name validation matches the control plane corpus", () => {
    for (const name of ["config-json", "0abc", "a.b", "A_B", "CON"]) {
      expect(() => validateSecretName(name)).not.toThrow()
    }
    for (const name of ["", "-x", "_x", "bad/name", "bad name", "a".repeat(129)]) {
      expect(() => validateSecretName(name)).toThrow()
    }
  })

  test("rejects malformed secret placements during compile", () => {
    expect(() =>
      compile({
        task: task({
          id: "bad-secret",
          sandbox: sandbox("test")
            .image(image("test").from("debian:trixie-slim"))
            .workspace("/app"),
          secrets: [{ name: "broken", env: "TOKEN", file: "/tmp/secret" } as never],
          run: async () => undefined,
        }),
        modulePath: "tasks/bad-secret.ts",
      }),
    ).toThrow("task secrets.0 must be { env: string }")
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
          secrets: [{ name: "token", env: " " }],
        }),
        modulePath: "tasks/blank-secret-target.ts",
      }),
    ).toThrow("task secrets.0 must be { env: string }")
    expect(() =>
      compile({
        task: task({
          ...base,
          secrets: [{ name: "token", env: "1TOKEN" }],
        }),
        modulePath: "tasks/blank-secret-target.ts",
      }),
    ).toThrow("task secrets.0.env must match /^[A-Za-z_][A-Za-z0-9_]*$/")
    expect(() =>
      compile({
        task: task({
          ...base,
          secrets: [{ name: "token", file: "" }],
        }),
        modulePath: "tasks/blank-secret-target.ts",
      }),
    ).toThrow("task secrets.0 must be { file: string, mode?: string, owner?: string }")
    expect(() =>
      compile({
        task: task({
          ...base,
          secrets: [{ name: "token", dir: "\t" }],
        }),
        modulePath: "tasks/blank-secret-target.ts",
      }),
    ).toThrow("task secrets.0 must be { dir: string, mode?: string, owner?: string }")
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
          secrets: [{ name: "token", file: "a/../secret" }],
        }),
        modulePath: "tasks/unsafe-secret-target.ts",
      }),
    ).toThrow("task secrets.0.file must not contain parent components")
    expect(() =>
      compile({
        task: task({
          ...base,
          secrets: [{ name: "token", dir: "/" }],
        }),
        modulePath: "tasks/unsafe-secret-target.ts",
      }),
    ).toThrow("task secrets.0.dir must target a file or directory")
    expect(() =>
      compile({
        task: task({
          ...base,
          secrets: [{ name: "token", file: " /tmp/secret" }],
        }),
        modulePath: "tasks/unsafe-secret-target.ts",
      }),
    ).toThrow("task secrets.0.file must not contain leading or trailing whitespace")
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
          secrets: [{ name: "token", file: "/tmp/secret", mode: "not-octal" }],
        }),
        modulePath: "tasks/bad-secret-mode.ts",
      }),
    ).toThrow("task secrets.0.mode must be an octal permission mode")
    expect(() =>
      compile({
        task: task({
          ...base,
          secrets: [{ name: "token", dir: "/tmp/secrets", mode: "1777" }],
        }),
        modulePath: "tasks/bad-secret-mode.ts",
      }),
    ).toThrow("task secrets.0.mode must only contain permission bits")
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

  test("sandbox network policy is emitted", () => {
    const bundle = compile({
      task: task({
        id: "network-policy",
        sandbox: sandbox("network-policy")
          .image(image("network-policy").from("debian:trixie-slim"))
          .workspace("/workspace")
          .network({ internet: { deny: ["10.0.0.0/8", "169.254.0.0/16"] } }),
        run: async () => undefined,
      }),
      modulePath: "tasks/network-policy.ts",
    })

    expect(bundle.sandbox?.network).toMatchObject({
      internet: true,
      deny: ["10.0.0.0/8", "169.254.0.0/16"],
    })
  })

  test("sandbox network can disable internet", () => {
    const bundle = compile({
      task: task({
        id: "network-disabled",
        sandbox: sandbox("network-disabled")
          .image(image("network-disabled").from("debian:trixie-slim"))
          .workspace("/workspace")
          .network({ internet: false }),
        run: async () => undefined,
      }),
      modulePath: "tasks/network-disabled.ts",
    })

    expect(bundle.sandbox?.network?.internet).toBe(false)
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

const schema: PayloadSchema<{ readonly branch: string; readonly attempts: number }> = {
  "~standard": {
    version: 1,
    vendor: "test",
    validate(value) {
      return { value: value as { readonly branch: string; readonly attempts: number } }
    },
  },
}

export const payload = task({
  id: "payload",
  sandbox: smokeSandbox,
  payload: schema,
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

  test("adapter run parses payload before invoking task code", async () => {
    const cwd = await parseTaskFixture(
      "schema-payload",
      `import { smokeSandbox } from "../fixture/sandbox"
import { task, type PayloadSchema } from "@helmr/sdk"

const schema: PayloadSchema<{ readonly issue: string }, { readonly issue: number }> = {
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
}

export const schemaPayload = task({
  id: "schema-payload",
  sandbox: smokeSandbox,
  payload: schema,
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
        sessionId: ctx.session.id,
        taskId: ctx.task.id,
        workspacePath: ctx.workspace.path,
        projectPath: ctx.workspace.projectPath,
      })`,
    )
    const result = await runAdapterTask(cwd, "context", {
      runId: "run-context",
      taskContextJson: sampleTaskContextJSON({
        runId: "run-context",
        sessionId: "session-context",
        taskId: "context",
      }),
    })

    expect(result.status, result.stderr).toBe(0)
    expect(taskOutput(result)).toEqual({
      runId: "run-context",
      sessionId: "session-context",
      taskId: "context",
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

  test("adapter run emits waitpoints and fails on closed response stream", async () => {
    const cwd = await taskFixture(
      "needs-token",
      `(() => {
        return wait.forToken("token-1", { schema: { "~standard": { version: 1, vendor: "test", validate: (value: unknown) => ({ value }) } }, metadata: { repo: "helmr" }, tags: ["release", "channel:slack"] }).unwrap()
      })()`,
      `import { wait } from "@helmr/sdk"\n`,
    )
    const result = await runAdapterTask(cwd, "needs-token")

    expect(result.status).toBe(0)
    expect(taskExitCode(result)).toBe(1)
    expect(result.controlEvents[0]?.event).toMatchObject({
      case: "waitpointRequested",
      value: {
        correlationId: "1",
        kind: "token",
        paramsJson: JSON.stringify({ token_id: "token-1" }),
        metadataJson: JSON.stringify({ repo: "helmr" }),
        tags: ["release", "channel:slack"],
      },
    })
    const error = JSON.parse(result.stderr.trim())
    expect(error).toMatchObject({
      level: "error",
      message: "adapter response stream closed",
    })
  })

  test("adapter run surfaces wait timeout errors from host-driven timeout responses", async () => {
    const cwd = await taskFixture(
      "token-timeout",
      `(async () => {
        const result = await wait.forToken("token-1", { schema: { "~standard": { version: 1, vendor: "test", validate: (value: unknown) => ({ value }) } }, timeout: 1 })
        try {
          result.unwrap()
          return { ok: false }
        } catch (error) {
          return { name: error instanceof Error ? error.name : String(error), message: error instanceof Error ? error.message : String(error) }
        }
      })()`,
      `import { wait } from "@helmr/sdk"\n`,
    )
    const result = await runAdapterTaskInteractively(
      cwd,
      "token-timeout",
      async ({ stdin, waitForControlEvent }) => {
        await waitForControlEvent("waitpointRequested")
        stdin.write(resumeDecisionFrame({
          waitpointId: "waitpoint-1",
          kind: "timed_out",
          dataJson: "null",
        }))
        stdin.end()
      },
    )

    expect(result.status, result.stderr).toBe(0)
    expect(taskOutput(result)).toMatchObject({
      name: "WaitTimeoutError",
      message: "waitpoint timed out after 1",
    })
  })

  test("adapter run emits wait.for requests", async () => {
    const cwd = await taskFixture(
      "needs-wait-for",
      "wait.for({ seconds: 10 })",
      `import { wait } from "@helmr/sdk"\n`,
    )
    const result = await runAdapterTask(cwd, "needs-wait-for")

    expect(result.status).toBe(0)
    expect(taskExitCode(result)).toBe(1)
    const event = result.controlEvents[0]?.event
    expect(event).toMatchObject({
      case: "waitpointRequested",
      value: {
        correlationId: "1",
        kind: "timer",
        paramsJson: JSON.stringify({ seconds: 10 }),
        timeout: 10,
      },
    })
    if (event?.case !== "waitpointRequested") throw new Error("expected waitpointRequested")
  })

  test("adapter run rounds millisecond wait.for requests up to seconds", async () => {
    const cwd = await taskFixture("needs-wait-for-ms", "wait.for({ milliseconds: 1500 })", `import { wait } from "@helmr/sdk"\n`)
    const result = await runAdapterTask(cwd, "needs-wait-for-ms")

    expect(result.status).toBe(0)
    expect(taskExitCode(result)).toBe(1)
    expect(result.controlEvents[0]?.event).toMatchObject({
      case: "waitpointRequested",
      value: {
        correlationId: "1",
        kind: "timer",
        paramsJson: JSON.stringify({ milliseconds: 1500 }),
        timeout: 2,
      },
    })
  })

  test("adapter run accepts duration wait.for requests", async () => {
    const cwd = await taskFixture("needs-wait-for-duration", "wait.for('1.5s')", `import { wait } from "@helmr/sdk"\n`)
    const result = await runAdapterTask(cwd, "needs-wait-for-duration")

    expect(result.status).toBe(0)
    expect(taskExitCode(result)).toBe(1)
    expect(result.controlEvents[0]?.event).toMatchObject({
      case: "waitpointRequested",
      value: {
        correlationId: "1",
        kind: "timer",
        paramsJson: JSON.stringify({ duration: "1.5s" }),
        timeout: 2,
      },
    })
  })

  test("adapter run emits wait.until requests", async () => {
    const cwd = await taskFixture(
      "needs-wait-until",
      "wait.until({ date: new Date('2026-04-23T00:00:00Z') })",
      `import { wait } from "@helmr/sdk"\n`,
    )
    const result = await runAdapterTask(cwd, "needs-wait-until")

    expect(result.status).toBe(0)
    expect(taskExitCode(result)).toBe(1)
    expect(result.controlEvents[0]?.event).toMatchObject({
      case: "waitpointRequested",
      value: {
        correlationId: "1",
        kind: "timer",
        paramsJson: JSON.stringify({ date: "2026-04-23T00:00:00.000Z" }),
        timeout: 1,
      },
    })
  })

  test("adapter run resolves waitpoint completions", async () => {
    const cwd = await taskFixture(
      "wait-human",
      `(() => {
        return wait.forToken("token-1", { schema: { "~standard": { version: 1, vendor: "test", validate: (value: unknown) => ({ value }) } } }).unwrap()
      })()`,
      `import { wait } from "@helmr/sdk"\n`,
    )
    const result = await runAdapterTaskInteractively(
      cwd,
      "wait-human",
      async ({ stdin, waitForControlEvent }) => {
        await waitForControlEvent("waitpointRequested")
        stdin.write(resumeDecisionFrame({
          waitpointId: "waitpoint-1",
          kind: "completed",
          dataJson: JSON.stringify({ ok: true }),
        }))
        stdin.end()
      },
    )

    expect(result.status, result.stderr).toBe(0)
    expect(result.controlEvents[0]?.event).toMatchObject({
      case: "waitpointRequested",
      value: {
        correlationId: "1",
        kind: "token",
        paramsJson: JSON.stringify({ token_id: "token-1" }),
      },
    })
    expect(taskOutput(result)).toEqual({ ok: true })
  })

  test("adapter run resolves channel waits as direct channel data", async () => {
    const cwd = await taskFixture(
      "wait-session-channel",
      `(() => {
        const approval = ctx.session.input(channels.input("approval", { schema: { "~standard": { version: 1, vendor: "test", validate: (value: unknown) => ({ value }) } } }))
        return approval.wait({ timeout: 5, tags: ["approval"] }).unwrap()
      })()`,
      `import { channels } from "@helmr/sdk"\n`,
    )
    const result = await runAdapterTaskInteractively(
      cwd,
      "wait-session-channel",
      async ({ stdin, waitForControlEvent }) => {
        await waitForControlEvent("waitpointRequested")
        stdin.write(resumeDecisionFrame({
          waitpointId: "waitpoint-1",
          kind: "completed",
          dataJson: JSON.stringify({ channel: "approval", sequence: 1, data: { approved: true } }),
        }))
        stdin.end()
      },
    )

    expect(result.status, result.stderr).toBe(0)
    expect(result.controlEvents[0]?.event).toMatchObject({
      case: "waitpointRequested",
      value: {
        correlationId: "1",
        kind: "channel",
        paramsJson: JSON.stringify({ channel: "approval" }),
        tags: ["approval"],
        timeout: 5,
      },
    })
    expect(taskOutput(result)).toEqual({ approved: true })
  })

  test("adapter run emits wait.all as one ordered aggregate wait", async () => {
    const cwd = await taskFixture(
      "wait-all",
      `(() => {
        const approval = ctx.session.input(channels.input("approval", { schema: { "~standard": { version: 1, vendor: "test", validate: (value: unknown) => ({ value }) } } }))
        return wait.all([
          wait.for({ seconds: 1 }),
          approval.wait({ correlationId: "thread-1" }),
        ])
      })()`,
      `import { channels, wait } from "@helmr/sdk"\n`,
    )
    const result = await runAdapterTaskInteractively(
      cwd,
      "wait-all",
      async ({ stdin, waitForControlEventCount }) => {
        await waitForControlEventCount("waitpointRequested", 2)
        stdin.write(resumeDecisionFrame({
          waitpointId: "waitpoint-1",
          kind: "waitpoints",
          dataJson: JSON.stringify({
            waitpoints: [
              null,
              { channel: "approval", sequence: 1, data: { approved: true } },
            ],
          }),
        }))
        stdin.end()
      },
    )

    expect(result.status, result.stderr).toBe(0)
    const waitpointEvents = result.controlEvents
      .map((event) => event.event)
      .filter((event): event is { case: "waitpointRequested"; value: runProto.WaitpointRequested } => event.case === "waitpointRequested")
    expect(waitpointEvents).toHaveLength(2)
    expect(waitpointEvents[0]?.value).toMatchObject({
      correlationId: "1",
      kind: "timer",
      paramsJson: JSON.stringify({ seconds: 1 }),
      timeout: 1,
      ordinal: 0,
    })
    expect(waitpointEvents[1]?.value).toMatchObject({
      correlationId: "1",
      kind: "channel",
      paramsJson: JSON.stringify({ channel: "approval", correlation_id: "thread-1" }),
      ordinal: 1,
    })
    expect(taskOutput(result)).toEqual([null, { approved: true }])
  })

  test("adapter run leaves channel wait cursors to the control plane", async () => {
    const cwd = await taskFixture(
      "wait-session-channel-twice",
      `(async () => {
        const numbers = ctx.session.input(channels.input("numbers", { schema: { "~standard": { version: 1, vendor: "test", validate: (value: unknown) => ({ value }) } } }))
        const first = await numbers.wait().unwrap()
        const second = await numbers.wait().unwrap()
        return [first, second]
      })()`,
      `import { channels } from "@helmr/sdk"\n`,
    )
    const result = await runAdapterTaskInteractively(
      cwd,
      "wait-session-channel-twice",
      async ({ stdin, waitForControlEvent }) => {
        await waitForControlEvent("waitpointRequested")
        stdin.write(resumeDecisionFrame({
          waitpointId: "waitpoint-1",
          kind: "completed",
          dataJson: JSON.stringify({ channel: "numbers", sequence: 1, data: 10 }),
        }))
        await waitForControlEvent("waitpointRequested")
        stdin.write(resumeDecisionFrame({
          waitpointId: "waitpoint-2",
          kind: "completed",
          dataJson: JSON.stringify({ channel: "numbers", sequence: 2, data: 20 }),
        }))
        stdin.end()
      },
    )

    expect(result.status, result.stderr).toBe(0)
    expect(result.controlEvents[0]?.event).toMatchObject({
      case: "waitpointRequested",
      value: {
        correlationId: "1",
        kind: "channel",
        paramsJson: JSON.stringify({ channel: "numbers" }),
      },
    })
    expect(result.controlEvents[1]?.event).toMatchObject({
      case: "waitpointRequested",
      value: {
        correlationId: "2",
        kind: "channel",
        paramsJson: JSON.stringify({ channel: "numbers" }),
      },
    })
    expect(taskOutput(result)).toEqual([10, 20])
  })

  test("adapter run preserves correlation ids without local channel cursors", async () => {
    const cwd = await taskFixture(
      "wait-session-channel-correlated",
      `(async () => {
        const replies = ctx.session.input(channels.input("replies", { schema: { "~standard": { version: 1, vendor: "test", validate: (value: unknown) => ({ value }) } } }))
        const firstA = await replies.wait({ correlationId: "thread-a" }).unwrap()
        const firstB = await replies.wait({ correlationId: "thread-b" }).unwrap()
        const secondA = await replies.wait({ correlationId: " thread-a " }).unwrap()
        return [firstA, firstB, secondA]
      })()`,
      `import { channels } from "@helmr/sdk"\n`,
    )
    const result = await runAdapterTaskInteractively(
      cwd,
      "wait-session-channel-correlated",
      async ({ stdin, waitForControlEvent }) => {
        await waitForControlEvent("waitpointRequested")
        stdin.write(resumeDecisionFrame({
          waitpointId: "waitpoint-1",
          kind: "completed",
          dataJson: JSON.stringify({ channel: "replies", correlation_id: "thread-a", sequence: 10, data: "a1" }),
        }))
        await waitForControlEvent("waitpointRequested")
        stdin.write(resumeDecisionFrame({
          waitpointId: "waitpoint-2",
          kind: "completed",
          dataJson: JSON.stringify({ channel: "replies", correlation_id: "thread-b", sequence: 3, data: "b1" }),
        }))
        await waitForControlEvent("waitpointRequested")
        stdin.write(resumeDecisionFrame({
          waitpointId: "waitpoint-3",
          kind: "completed",
          dataJson: JSON.stringify({ channel: "replies", correlation_id: "thread-a", sequence: 11, data: "a2" }),
        }))
        stdin.end()
      },
    )

    expect(result.status, result.stderr).toBe(0)
    expect(result.controlEvents[0]?.event).toMatchObject({
      case: "waitpointRequested",
      value: {
        correlationId: "1",
        kind: "channel",
        paramsJson: JSON.stringify({ channel: "replies", correlation_id: "thread-a" }),
      },
    })
    expect(result.controlEvents[1]?.event).toMatchObject({
      case: "waitpointRequested",
      value: {
        correlationId: "2",
        kind: "channel",
        paramsJson: JSON.stringify({ channel: "replies", correlation_id: "thread-b" }),
      },
    })
    expect(result.controlEvents[2]?.event).toMatchObject({
      case: "waitpointRequested",
      value: {
        correlationId: "3",
        kind: "channel",
        paramsJson: JSON.stringify({ channel: "replies", correlation_id: "thread-a" }),
      },
    })
    expect(taskOutput(result)).toEqual(["a1", "b1", "a2"])
  })

  test("adapter run parses waitpoint completions with validation-only schemas", async () => {
    const cwd = await taskFixture(
      "wait-human-validation-schema",
      `(async () => {
        const schema: PayloadSchema<unknown, { readonly approved: boolean }> = {
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
        return await wait.forToken("token-1", { schema }).unwrap()
      })()`,
      `import { wait, type PayloadSchema } from "@helmr/sdk"\n`,
    )
    const result = await runAdapterTaskInteractively(
      cwd,
      "wait-human-validation-schema",
      async ({ stdin, waitForControlEvent }) => {
        await waitForControlEvent("waitpointRequested")
        stdin.write(resumeDecisionFrame({
          waitpointId: "waitpoint-1",
          kind: "completed",
          dataJson: JSON.stringify({ approved: true }),
        }))
        stdin.end()
      },
    )

    expect(result.status, result.stderr).toBe(0)
    expect(taskOutput(result)).toEqual({ approved: true })
  })

  test("adapter run rejects invalid waitpoint completion data", async () => {
    const cwd = await taskFixture(
      "token-invalid-json",
      `(async () => {
        try {
          return await wait.forToken("token-1", { schema: { "~standard": { version: 1, vendor: "test", validate: (value: unknown) => ({ value }) } } }).unwrap()
        } catch (error) { return { message: error instanceof Error ? error.message : String(error) } }
      })()`,
      `import { wait } from "@helmr/sdk"\n`,
    )
    const result = await runAdapterTaskInteractively(
      cwd,
      "token-invalid-json",
      async ({ stdin, waitForControlEvent }) => {
        await waitForControlEvent("waitpointRequested")
        stdin.write(resumeDecisionFrame({
          waitpointId: "waitpoint-1",
          kind: "completed",
          dataJson: "{",
        }))
        stdin.end()
      },
    )

    expect(result.status, result.stderr).toBe(0)
    expect(taskOutput(result).message).toContain("waitpoint data must be valid JSON")
  })

  test("adapter run returns null waitpoint completion data", async () => {
    const cwd = await taskFixture(
      "token-null-data",
      `(() => wait.forToken("token-1").unwrap())()`,
      `import { wait } from "@helmr/sdk"\n`,
    )
    const result = await runAdapterTaskInteractively(
      cwd,
      "token-null-data",
      async ({ stdin, waitForControlEvent }) => {
        await waitForControlEvent("waitpointRequested")
        stdin.write(resumeDecisionFrame({
          waitpointId: "waitpoint-1",
          kind: "completed",
          dataJson: "null",
        }))
        stdin.end()
      },
    )

    expect(result.status, result.stderr).toBe(0)
    expect(taskOutput(result)).toBeNull()
  })

  test("adapter run validates null waitpoint completion data as direct data", async () => {
    const cwd = await taskFixture(
      "token-null-data-schema",
      `(async () => {
        try {
          return await wait.forToken("token-1", { schema: { "~standard": { version: 1, vendor: "test", validate(value: unknown) {
            if (value === null || typeof value !== "object") {
              return { issues: [{ message: "expected object" }] }
            }
            return { value }
          } } } }).unwrap()
        } catch (error) { return { message: error instanceof Error ? error.message : String(error) } }
      })()`,
      `import { wait } from "@helmr/sdk"\n`,
    )
    const result = await runAdapterTaskInteractively(
      cwd,
      "token-null-data-schema",
      async ({ stdin, waitForControlEvent }) => {
        await waitForControlEvent("waitpointRequested")
        stdin.write(resumeDecisionFrame({
          waitpointId: "waitpoint-1",
          kind: "completed",
          dataJson: "null",
        }))
        stdin.end()
      },
    )

    expect(result.status, result.stderr).toBe(0)
    expect(taskOutput(result).message).toContain("waitpoint data failed validation: expected object")
  })

  test("adapter run rejects old waitpoint completion wrappers", async () => {
    const cwd = await taskFixture(
      "token-old-wrapper",
      `(async () => {
        try {
          return await wait.forToken("token-1", { schema: { "~standard": { version: 1, vendor: "test", validate(value: unknown) {
            if (value === null || typeof value !== "object" || typeof (value as Record<string, unknown>).approved !== "boolean") {
              return { issues: [{ message: "expected approval object", path: ["approved"] }] }
            }
            return { value }
          } } } }).unwrap()
        } catch (error) { return { message: error instanceof Error ? error.message : String(error) } }
      })()`,
      `import { wait } from "@helmr/sdk"\n`,
    )
    const result = await runAdapterTaskInteractively(
      cwd,
      "token-old-wrapper",
      async ({ stdin, waitForControlEvent }) => {
        await waitForControlEvent("waitpointRequested")
        stdin.write(resumeDecisionFrame({
          waitpointId: "waitpoint-1",
          kind: "completed",
          dataJson: JSON.stringify({
            payload: { approved: true },
            at: "2026-04-23T00:00:00Z",
          }),
        }))
        stdin.end()
      },
    )

    expect(result.status, result.stderr).toBe(0)
    expect(taskOutput(result).message).toContain("waitpoint data failed validation: payload.approved: expected approval object")
  })

  test("adapter run rejects resume decisions with the wrong kind for waitpoints", async () => {
    const cwd = await taskFixture(
      "token-wrong-kind",
      `(() => {
        return wait.forToken("token-1", { schema: { "~standard": { version: 1, vendor: "test", validate: (value: unknown) => ({ value }) } } }).unwrap()
      })()`,
      `import { wait } from "@helmr/sdk"\n`,
    )
    const result = await runAdapterTaskInteractively(
      cwd,
      "token-wrong-kind",
      async ({ stdin, waitForControlEvent }) => {
        await waitForControlEvent("waitpointRequested")
        stdin.write(resumeDecisionFrame({
          waitpointId: "waitpoint-1",
          kind: "unexpected",
          dataJson: JSON.stringify({ principal: "alice" }),
        }))
        stdin.end()
      },
    )

    expect(result.status).toBe(0)
    expect(taskExitCode(result)).toBe(1)
    expect(JSON.parse(result.stderr.trim()).message).toBe('unexpected waitpoint resume decision kind "unexpected"')
  })

  test("adapter run rejects concurrent waits with ConcurrentWaitError", async () => {
    const cwd = await taskFixture(
      "concurrent-wait",
      `(async () => {
        const first = wait.forToken("token-1", { schema: { "~standard": { version: 1, vendor: "test", validate: (value: unknown) => ({ value }) } } }).unwrap().catch(() => undefined)
        try {
          await wait.forToken("token-2", { schema: { "~standard": { version: 1, vendor: "test", validate: (value: unknown) => ({ value }) } } }).unwrap()
          return { ok: false }
        } catch (error) {
          return { concurrent: error instanceof ConcurrentWaitError, name: error instanceof Error ? error.name : String(error), message: error instanceof Error ? error.message : String(error) }
        } finally { await first }
      })()`,
      "import { ConcurrentWaitError, wait } from \"@helmr/sdk\"\n",
    )
    const result = await runAdapterTask(cwd, "concurrent-wait")

    expect(result.status, result.stderr).toBe(0)
    expect(result.controlEvents[0]?.event.case).toBe("waitpointRequested")
    expect(taskOutput(result)).toMatchObject({
      concurrent: true,
      name: "ConcurrentWaitError",
      message: "concurrent blocking run I/O calls are not supported",
    })
  })

  test("adapter run rejects empty waitpoint tags before emitting control events", async () => {
    const cwd = await taskFixture(
      "oversized-wait",
      `wait.forToken("token-1", { schema: { "~standard": { version: 1, vendor: "test", validate: (value: unknown) => ({ value }) } }, tags: [""] }).unwrap()`,
      `import { wait } from "@helmr/sdk"\n`,
    )
    const result = await runAdapterTask(cwd, "oversized-wait")

    expect(result.status).toBe(0)
    expect(taskExitCode(result)).toBe(1)
    expect(result.controlEvents.map((event) => event.event.case)).not.toContain("waitpointRequested")
    const error = JSON.parse(result.stderr.trim())
    expect(error.message).toContain("wait tag")
    expect(error.message).toContain("must be non-empty")
  })

  test("adapter run rejects too many waitpoint tags before emitting control events", async () => {
    const cwd = await taskFixture(
      "too-many-wait-tags",
      `wait.forToken("token-1", { schema: { "~standard": { version: 1, vendor: "test", validate: (value: unknown) => ({ value }) } }, tags: Array.from({ length: 33 }, (_, index) => "tag-" + index) }).unwrap()`,
      `import { wait } from "@helmr/sdk"\n`,
    )
    const result = await runAdapterTask(cwd, "too-many-wait-tags")

    expect(result.status).toBe(0)
    expect(taskExitCode(result)).toBe(1)
    expect(result.controlEvents.map((event) => event.event.case)).not.toContain("waitpointRequested")
    const error = JSON.parse(result.stderr.trim())
    expect(error.message).toContain("wait tags")
    expect(error.message).toContain("exceeds max")
  })

  test("adapter run rejects oversized waitpoint tags before emitting control events", async () => {
    const cwd = await taskFixture(
      "oversized-wait-tag",
      `wait.forToken("token-1", { schema: { "~standard": { version: 1, vendor: "test", validate: (value: unknown) => ({ value }) } }, tags: ["${"x".repeat(129)}"] }).unwrap()`,
      `import { wait } from "@helmr/sdk"\n`,
    )
    const result = await runAdapterTask(cwd, "oversized-wait-tag")

    expect(result.status).toBe(0)
    expect(taskExitCode(result)).toBe(1)
    expect(result.controlEvents.map((event) => event.event.case)).not.toContain("waitpointRequested")
    const error = JSON.parse(result.stderr.trim())
    expect(error.message).toContain("wait tag")
    expect(error.message).toContain("exceeds max")
  })

  test("adapter run rejects non-object waitpoint metadata before emitting control events", async () => {
    const cwd = await taskFixture(
      "invalid-input-metadata",
      `wait.forToken("token-1", { schema: { "~standard": { version: 1, vendor: "test", validate: (value: unknown) => ({ value }) } }, metadata: [] } as any).unwrap()`,
      `import { wait } from "@helmr/sdk"\n`,
    )
    const result = await runAdapterTask(cwd, "invalid-input-metadata")

    expect(result.status).toBe(0)
    expect(taskExitCode(result)).toBe(1)
    expect(result.controlEvents.map((event) => event.event.case)).not.toContain("waitpointRequested")
    const error = JSON.parse(result.stderr.trim())
    expect(error.message).toBe("wait metadata must be a JSON object")
  })

  test("adapter run rejects oversized waitpoint metadata before emitting control events", async () => {
    const cwd = await taskFixture(
      "oversized-input-metadata",
      `wait.forToken("token-1", { schema: { "~standard": { version: 1, vendor: "test", validate: (value: unknown) => ({ value }) } }, metadata: { value: "x".repeat(70 * 1024) } }).unwrap()`,
      `import { wait } from "@helmr/sdk"\n`,
    )
    const result = await runAdapterTask(cwd, "oversized-input-metadata")

    expect(result.status).toBe(0)
    expect(taskExitCode(result)).toBe(1)
    expect(result.controlEvents.map((event) => event.event.case)).not.toContain("waitpointRequested")
    const error = JSON.parse(result.stderr.trim())
    expect(error.message).toContain("wait metadata_json")
    expect(error.message).toContain("exceeds max")
  })

  test("adapter run emits durable channel output appends", async () => {
    const cwd = await taskFixture(
      "writes-channel-output",
      "(() => { ctx.session.output(channels.output('agent.report', { schema: { '~standard': { version: 1, vendor: 'test', validate: (value: unknown) => ({ value }) } } })).append({ ok: true }, { contentType: 'application/json', objectRef: { path: 'report.json' } }); return { ok: true } })()",
      `import { channels } from "@helmr/sdk"\n`,
    )
    const result = await runAdapterTask(cwd, "writes-channel-output")

    expect(result.status, result.stderr).toBe(0)
    expect(result.controlEvents[0]?.event).toMatchObject({
      case: "channelOutputAppended",
      value: {
        channel: "agent.report",
        payloadJson: JSON.stringify({ ok: true }),
        contentType: "application/json",
        objectRefJson: JSON.stringify({ path: "report.json" }),
      },
    })
  })

  test("adapter run emits metadata updates", async () => {
    const cwd = await taskFixture(
      "writes-metadata",
      "(() => { metadata.set('status', 'reviewing'); metadata.patch({ currentFile: 'src/app.ts' }); metadata.increment('filesReviewed', 1); return { ok: true } })()",
      `import { metadata } from "@helmr/sdk"\n`,
    )
    const result = await runAdapterTask(cwd, "writes-metadata")

    expect(result.status, result.stderr).toBe(0)
    expect(result.controlEvents.slice(0, 3).map((event) => event.event)).toMatchObject([
      { case: "metadataUpdated", value: { operation: "set", key: "status", valueJson: JSON.stringify("reviewing") } },
      { case: "metadataUpdated", value: { operation: "patch", patchJson: JSON.stringify({ currentFile: "src/app.ts" }) } },
      { case: "metadataUpdated", value: { operation: "increment", key: "filesReviewed", amount: 1 } },
    ])
  })

  test("adapter run truncates oversized logger entries", async () => {
    const cwd = await taskFixture(
      "oversized-log",
      "(() => { logger.info('" + "x".repeat(70 * 1024) + "'); return { ok: true } })()",
      `import { logger } from "@helmr/sdk"\n`,
    )
    const result = await runAdapterTask(cwd, "oversized-log")

    expect(result.status, result.stderr).toBe(0)
    expect(result.controlEvents[0]?.event.case).toBe("logEntry")
    const entry = result.controlEvents[0]?.event.value
    expect(typeof entry).toBe("string")
    expect(Buffer.byteLength(entry as string, "utf8")).toBeLessThanOrEqual(64 * 1024)
    const payload = JSON.parse(entry as string)
    expect(payload.message).toContain("...[truncated logger entry]")
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
    .resources({ cpu: 2, memory: "4Gi", disk: "32Gi" })

  return task({
    id: "hello",
    sandbox: smokeSandbox,
    maxDuration: 5 * 60,
    secrets: [{ name: "githubToken", env: "GITHUB_TOKEN" }],
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
    readonly waitForControlEventCount: (kind: runProto.RunEvent["event"]["case"], count: number) => Promise<void>
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
    await waitForControlEventCount(kind, 1)
  }
  const waitForControlEventCount = async (kind: runProto.RunEvent["event"]["case"], count: number): Promise<void> => {
    for (let attempt = 0; attempt < 100; attempt += 1) {
      if (decodeRunEvents(control.bytes()).filter((event) => event.event.case === kind).length >= count) {
        return
      }
      await new Promise((resolve) => setTimeout(resolve, 5))
    }
    throw new Error(`timed out waiting for ${count} control event(s): ${kind}`)
  }

  await interact({ stdin, waitForControlEvent, waitForControlEventCount })
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
  readonly sessionId?: string
  readonly taskId: string
}): string {
  return JSON.stringify({
    run: { id: options.runId },
    task: { id: options.taskId },
    workspace: {
      path: "/workspace",
      projectPath: "/workspace",
    },
    session: {
      id: options.sessionId ?? "session-1",
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
  readonly dataJson?: string
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

function taskOutput(run: { readonly controlEvents: readonly runProto.RunEvent[] }): unknown {
  const result = taskResult(run)
  if (result.outputJson === undefined) {
    throw new Error("missing task output event")
  }
  return JSON.parse(result.outputJson)
}

function taskExitCode(run: { readonly controlEvents: readonly runProto.RunEvent[] }): number {
  return taskResult(run).exitCode
}

function taskErrorMessage(run: { readonly controlEvents: readonly runProto.RunEvent[] }): string {
  const message = taskResult(run).errorMessage
  if (message === undefined) {
    throw new Error("missing task result error message")
  }
  return message
}

function taskResult(run: { readonly controlEvents: readonly runProto.RunEvent[] }): runProto.TaskResult {
  const event = run.controlEvents.find((event) => event.event.case === "taskResult")
  if (event?.event.case !== "taskResult") {
    throw new Error("missing task result event")
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
