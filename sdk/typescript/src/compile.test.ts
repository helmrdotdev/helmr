import { describe, expect, test } from "bun:test"
import { create, fromBinary, toBinary } from "@bufbuild/protobuf"
import { createHash } from "node:crypto"
import { mkdir, mkdtemp, realpath, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { resolve } from "node:path"
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
import { image, sandbox, source, task } from "./index"

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
    expect(image.formatVersion).toBe(1)
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
      "sha256:b175ca69bc544e1e4ac005d921a6822171eb588ba7d92035e3048abeaa1c3da7",
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
      `({ cwd: process.cwd(), workspace: await Bun.file("workspace.txt").text() })`,
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
    const cwd = await taskFixture("payload", "({ payload: _payload, runId: ctx.run.id })")
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

  test("adapter run discovers tasks by id instead of filename", async () => {
    const cwd = await mkdtemp(resolve(tmpdir(), "helmr-adapter-module-test-"))
    await mkdir(resolve(cwd, "tasks/custom"), { recursive: true })
    await writeFile(
      resolve(cwd, "tasks/custom/review.ts"),
      'import { image, sandbox, task } from "@helmr/sdk"\nconst sb = sandbox("hello").image(image("hello").from("debian:trixie-slim")).workspace("/app")\nexport const review = task({ id: "hello", sandbox: sb, run: async () => ({ module: "custom" }) })\n',
    )
    await writeFile(
      resolve(cwd, "helmr.config.ts"),
      'import { defineConfig } from "@helmr/sdk"\nexport default defineConfig({ dirs: ["./tasks"] })\n',
    )
    const result = await runAdapterTask(cwd, "hello")

    expect(result.status, result.stderr).toBe(0)
    expect(taskOutput(result)).toEqual({ module: "custom" })
  })

  test("adapter run reports a fuzzy suggestion when task id is missing", async () => {
    const cwd = await taskFixture("codex-review", "({ ok: true })", "", "review.ts")
    const result = await runAdapterTask(cwd, "codex-reveiw")

    expect(result.status).toBe(1)
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

    expect(result.status).toBe(1)
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
      'import { defineConfig } from "@helmr/sdk"\nexport default defineConfig({ dirs: ["./tasks"] })\n',
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

  test("adapter run emits approval requests and fails on closed response stream", async () => {
    const cwd = await taskFixture("needs-approval", "ctx.wait.approval('ship it')")
    const result = await runAdapterTask(cwd, "needs-approval")

    expect(result.status).toBe(1)
    expect(result.controlEvents[0]?.event).toMatchObject({
      case: "waitRequested",
      value: {
        correlationId: "1",
        kind: { case: "approval", value: { message: "ship it" } },
      },
    })
    const error = JSON.parse(result.stderr.trim())
    expect(error).toMatchObject({
      level: "error",
      message: "adapter response stream closed",
    })
  })

  test("adapter run surfaces ApprovalTimeoutError from host-driven timeout responses", async () => {
    const cwd = await taskFixture(
      "approval-timeout",
      "(async () => { try { await ctx.wait.approval('ship it', { timeout: 1 }); return { ok: false } } catch (error) { return { timeout: error instanceof ApprovalTimeoutError, name: error instanceof Error ? error.name : String(error), message: error instanceof Error ? error.message : String(error) } } })()",
      "import { ApprovalTimeoutError } from \"@helmr/sdk\"\n",
    )
    const result = await runAdapterTaskInteractively(
      cwd,
      "approval-timeout",
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
      timeout: true,
      name: "ApprovalTimeoutError",
      message: "approval timed out after 1",
    })
  })

  test("adapter run resolves approved approval waits from host decisions", async () => {
    const cwd = await taskFixture("approval-approved", "ctx.wait.approval('ship it', { timeout: 60 })")
    const result = await runAdapterTaskInteractively(
      cwd,
      "approval-approved",
      async ({ stdin, waitForControlEvent }) => {
        await waitForControlEvent("waitRequested")
        stdin.write(resumeDecisionFrame({
          waitpointId: "waitpoint-1",
          kind: "approved",
          resolutionPayloadJson: JSON.stringify({
            principal: "alice",
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
        kind: { case: "approval", value: { message: "ship it", timeout: 60 } },
      },
    })
    expect(taskOutput(result)).toEqual({
      approved: true,
      approvedBy: "alice",
      at: "2026-04-23T00:00:00.000Z",
    })
  })

  test("adapter run resolves denied approval waits from host decisions", async () => {
    const cwd = await taskFixture("approval-denied", "ctx.wait.approval('ship it')")
    const result = await runAdapterTaskInteractively(
      cwd,
      "approval-denied",
      async ({ stdin, waitForControlEvent }) => {
        await waitForControlEvent("waitRequested")
        stdin.write(resumeDecisionFrame({
          waitpointId: "waitpoint-1",
          kind: "denied",
          resolutionPayloadJson: JSON.stringify({
            principal: "bob",
            at: "2026-04-23T00:01:00Z",
          }),
        }))
        stdin.end()
      },
    )

    expect(result.status, result.stderr).toBe(0)
    expect(taskOutput(result)).toEqual({
      approved: false,
      approvedBy: "bob",
      at: "2026-04-23T00:01:00.000Z",
    })
  })

  test("adapter run emits message requests and fails on closed response stream", async () => {
    const cwd = await taskFixture("needs-message", "ctx.wait.message('next')")
    const result = await runAdapterTask(cwd, "needs-message")

    expect(result.status).toBe(1)
    expect(result.controlEvents[0]?.event).toMatchObject({
      case: "waitRequested",
      value: {
        correlationId: "1",
        kind: { case: "message", value: { prompt: "next" } },
      },
    })
    const error = JSON.parse(result.stderr.trim())
    expect(error).toMatchObject({
      level: "error",
      message: "adapter response stream closed",
    })
  })

  test("adapter run surfaces MessageTimeoutError from host-driven timeout responses", async () => {
    const cwd = await taskFixture(
      "message-timeout",
      "(async () => { try { await ctx.wait.message('next', { timeout: 1 }); return { ok: false } } catch (error) { return { timeout: error instanceof MessageTimeoutError, name: error instanceof Error ? error.name : String(error), message: error instanceof Error ? error.message : String(error) } } })()",
      "import { MessageTimeoutError } from \"@helmr/sdk\"\n",
    )
    const result = await runAdapterTaskInteractively(
      cwd,
      "message-timeout",
      async ({ stdin, waitForControlEvent }) => {
        await waitForControlEvent("waitRequested")
        stdin.write(resumeDecisionFrame({
          waitpointId: "waitpoint-1",
          kind: "timed_out",
          resolutionPayloadJson: JSON.stringify({ at: "2026-04-23T00:00:00Z", attachments: [] }),
          timedOut: true,
        }))
        stdin.end()
      },
    )

    expect(result.status, result.stderr).toBe(0)
    expect(taskOutput(result)).toMatchObject({
      timeout: true,
      name: "MessageTimeoutError",
      message: "message wait timed out after 1",
    })
  })

  test("adapter run resolves message waits from host replies", async () => {
    const cwd = await taskFixture("message-reply", "ctx.wait.message('next', { timeout: 30 })")
    const result = await runAdapterTaskInteractively(
      cwd,
      "message-reply",
      async ({ stdin, waitForControlEvent }) => {
        await waitForControlEvent("waitRequested")
        stdin.write(resumeDecisionFrame({
          waitpointId: "waitpoint-1",
          kind: "replied",
          resolutionPayloadJson: JSON.stringify({
            text: "continue",
            principal: "carol",
            at: "2026-04-23T00:02:00Z",
            attachments: [{ name: "notes.txt", url: "https://example.test/notes.txt" }],
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
        kind: { case: "message", value: { prompt: "next", timeout: 30 } },
      },
    })
    expect(taskOutput(result)).toEqual({
      text: "continue",
      sentBy: "carol",
      at: "2026-04-23T00:02:00.000Z",
      attachments: [{ name: "notes.txt", url: "https://example.test/notes.txt" }],
    })
  })

  test("adapter run rejects resume decisions with the wrong kind for the wait type", async () => {
    const approvalCwd = await taskFixture("approval-wrong-kind", "ctx.wait.approval('ship it')")
    const approvalResult = await runAdapterTaskInteractively(
      approvalCwd,
      "approval-wrong-kind",
      async ({ stdin, waitForControlEvent }) => {
        await waitForControlEvent("waitRequested")
        stdin.write(resumeDecisionFrame({
          waitpointId: "waitpoint-1",
          kind: "replied",
          resolutionPayloadJson: JSON.stringify({ text: "not approval" }),
        }))
        stdin.end()
      },
    )

    expect(approvalResult.status).toBe(1)
    expect(JSON.parse(approvalResult.stderr.trim()).message).toBe(
      'unexpected approval resume decision kind "replied"',
    )

    const messageCwd = await taskFixture("message-wrong-kind", "ctx.wait.message('next')")
    const messageResult = await runAdapterTaskInteractively(
      messageCwd,
      "message-wrong-kind",
      async ({ stdin, waitForControlEvent }) => {
        await waitForControlEvent("waitRequested")
        stdin.write(resumeDecisionFrame({
          waitpointId: "waitpoint-1",
          kind: "approved",
          resolutionPayloadJson: JSON.stringify({ principal: "alice" }),
        }))
        stdin.end()
      },
    )

    expect(messageResult.status).toBe(1)
    expect(JSON.parse(messageResult.stderr.trim()).message).toBe(
      'unexpected message resume decision kind "approved"',
    )
  })

  test("adapter run rejects concurrent waits with ConcurrentWaitError", async () => {
    const cwd = await taskFixture(
      "concurrent-wait",
      "(async () => { const first = ctx.wait.approval('one').catch(() => undefined); try { await ctx.wait.message('two'); return { ok: false } } catch (error) { return { concurrent: error instanceof ConcurrentWaitError, name: error instanceof Error ? error.name : String(error), message: error instanceof Error ? error.message : String(error) } } finally { await first } })()",
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

  test("adapter run rejects oversized wait prompts before emitting control events", async () => {
    const cwd = await taskFixture(
      "oversized-wait",
      `ctx.wait.approval(${JSON.stringify("x".repeat(16 * 1024 + 1))})`,
    )
    const result = await runAdapterTask(cwd, "oversized-wait")

    expect(result.status).toBe(1)
    expect(result.controlEvents).toHaveLength(0)
    const error = JSON.parse(result.stderr.trim())
    expect(error.message).toContain("approval wait message")
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

    expect(result.status).toBe(1)
    expect(result.controlEvents).toHaveLength(0)
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

    expect(result.status).toBe(1)
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

    expect(result.status).toBe(1)
    const error = JSON.parse(result.stderr.trim())
    expect(error.message).toContain("task module bundle failed")
    expect(error.message).toContain("Bundle failed")
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
    'import { defineConfig } from "@helmr/sdk"\nexport default defineConfig({ dirs: ["./tasks"] })\n',
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
    `${imports}import { image, sandbox, task } from "@helmr/sdk"\nconst sb = sandbox(${JSON.stringify(taskId)}).image(image(${JSON.stringify(taskId)}).from("debian:trixie-slim")).workspace("/app")\nexport const discoveredTask = task({ id: ${JSON.stringify(taskId)}, sandbox: sb, run: async (_payload, ctx) => ${expression} })\n`,
  )
  await writeFile(
    resolve(cwd, "helmr.config.ts"),
    'import { defineConfig } from "@helmr/sdk"\nexport default defineConfig({ dirs: ["./tasks"] })\n',
  )
  return cwd
}

async function taskFixtureWithExtension(
  taskId: string,
  expression: string,
  extension: ".mts" | ".cts" | ".tsx",
): Promise<string> {
  const cwd = await mkdtemp(resolve(tmpdir(), `helmr-adapter-${extension.slice(1)}-test-`))
  await mkdir(resolve(cwd, "tasks"))
  await writeFile(
    resolve(cwd, "tasks", `${taskId}${extension}`),
    `import { image, sandbox, task } from "@helmr/sdk"\nconst sb = sandbox(${JSON.stringify(taskId)}).image(image(${JSON.stringify(taskId)}).from("debian:trixie-slim")).workspace("/app")\nexport const discoveredTask = task({ id: ${JSON.stringify(taskId)}, sandbox: sb, run: async (_payload, ctx) => ${expression} })\n`,
  )
  await writeFile(
    resolve(cwd, "helmr.config.ts"),
    'import { defineConfig } from "@helmr/sdk"\nexport default defineConfig({ dirs: ["./tasks"] })\n',
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
  const previousAdapterSdkPath = process.env["HELMR_ADAPTER_SDK_PATH"]
  process.env["HELMR_ADAPTER_SDK_PATH"] = fileURLToPath(new URL("./index.ts", import.meta.url))
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

  try {
    await interact({ stdin, waitForControlEvent })
    const status = await resultPromise
    return {
      stdout: stdout.bytes().toString(),
      stderr: stderr.text(),
      status,
      controlEvents: decodeRunEvents(control.bytes()),
    }
  } finally {
    if (previousAdapterSdkPath === undefined) {
      delete process.env["HELMR_ADAPTER_SDK_PATH"]
    } else {
      process.env["HELMR_ADAPTER_SDK_PATH"] = previousAdapterSdkPath
    }
  }
}

interface RunAdapterTaskOptions {
  readonly runId?: string
  readonly payloadJson?: string
  readonly taskCwd?: string
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
  const previousAdapterSdkPath = process.env["HELMR_ADAPTER_SDK_PATH"]
  process.env["HELMR_ADAPTER_SDK_PATH"] = fileURLToPath(new URL("./index.ts", import.meta.url))
  const stdout = new CaptureSink()
  const stderr = new CaptureSink()
  const control = new CaptureSink()
  const io: AdapterIo = {
    stdin,
    stdout,
    stderr,
    control,
  }
  try {
    const status = await runAdapterCli(argv, io)
    return { stdout: stdout.bytes(), stderr: stderr.text(), status, control: control.bytes() }
  } finally {
    if (previousAdapterSdkPath === undefined) {
      delete process.env["HELMR_ADAPTER_SDK_PATH"]
    } else {
      process.env["HELMR_ADAPTER_SDK_PATH"] = previousAdapterSdkPath
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
  const event = result.controlEvents.find((event) => event.event.case === "taskOutput")
  if (event?.event.case !== "taskOutput") {
    throw new Error("missing task output event")
  }
  return JSON.parse(event.event.value.outputJson)
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

const IMAGE_KEY_DOMAIN = new TextEncoder().encode("helmr.image.v1\n")

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
