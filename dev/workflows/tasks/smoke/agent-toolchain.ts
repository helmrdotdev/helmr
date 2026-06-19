import { query, type Options as ClaudeOptions } from "@anthropic-ai/claude-agent-sdk"
import { Agent } from "@cursor/sdk"
import { cache, image, logger, sandbox, source, task } from "@helmr/sdk"
import { writeFile } from "node:fs/promises"
import { runCodex as runCodexTurn, type CodexThreadOptions } from "../lib/agents/codex-app-server"
import { DEFAULT_CLAUDE_MODEL, DEFAULT_CODEX_MODEL, DEFAULT_CURSOR_MODEL } from "../lib/agents/models"
import { baseAgentEnv } from "../lib/env"
import { renderAgentGuideInstruction } from "../lib/guides"
import { runCommand } from "../lib/process"
import { z } from "zod"

const dependencyInputs = source.directory(".", {
  ignore: ["*", "!package.json", "!bun.lock", "!tsconfig.json", "!vendor", "!vendor/**"],
})
const guideInputs = source.directory("guides")

const base = image("helmr-agent-toolchain-smoke")
  .from("node:24-bookworm-slim")
  .workdir("/workspace")
  .copy("/workspace", dependencyInputs)
  .copy("/opt/helmr-dev-workflows/guides", guideInputs)
  .run([
    "sh",
    "-ceu",
    [
      "apt-get update",
      "apt-get install -y --no-install-recommends ca-certificates curl xz-utils git gh ripgrep python3 make g++ util-linux",
      "rm -rf /var/lib/apt/lists/*",
    ].join(" && "),
  ])
  .run([
    "sh",
    "-ceu",
    [
      "mkdir -m 0755 -p /nix /etc/nix",
      "printf '%s\\n' 'build-users-group =' > /etc/nix/nix.conf",
      "curl -L https://releases.nixos.org/nix/nix-2.34.7/install | sh -s -- --no-daemon --no-channel-add",
      "printf '%s\\n' 'build-users-group =' 'experimental-features = nix-command flakes' 'accept-flake-config = true' 'sandbox = true' 'sandbox-fallback = false' > /etc/nix/nix.conf",
      "/root/.nix-profile/bin/nix --version",
    ].join(" && "),
  ])
  .env("PATH", "/root/.nix-profile/bin:/nix/var/nix/profiles/default/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
  .run(["npm", "install", "-g", "bun@1.3.10"])
  .run(["bun", "install", "--frozen-lockfile"], {
    cache: [{ mountPath: "/root/.bun/install/cache", cache: cache("agent-toolchain-smoke-bun") }],
  })

const sbx = sandbox("helmr-agent-toolchain-smoke")
  .image(base)
  .resources({ cpu: 2, memory: "4Gi", disk: "32Gi" })

interface Payload {
  readonly repository?: string
  readonly ref?: string
  readonly claudeModel?: string
  readonly codexModel?: string
  readonly cursorModel?: string
}

const payload = z.object({
  repository: z.string().optional(),
  ref: z.string().optional(),
  claudeModel: z.string().optional(),
  codexModel: z.string().optional(),
  cursorModel: z.string().optional(),
}).strict()

interface CheckResult {
  readonly name: string
  readonly ok: boolean
  readonly detail: unknown
}

export const agentToolchainSmoke = task({
  id: "agent-toolchain-smoke",
  sandbox: sbx,
  maxDuration: 1800,
  secrets: [
    { name: "ANTHROPIC_API_KEY", env: "ANTHROPIC_API_KEY" },
    { name: "OPENAI_API_KEY", env: "OPENAI_API_KEY" },
    { name: "CURSOR_API_KEY", env: "CURSOR_API_KEY" },
    { name: "GITHUB_TOKEN", env: "GITHUB_TOKEN" },
  ],
  payload,
  run: async (payload: Payload, ctx) => {
    const repository = payload.repository?.trim() || "helmrdotdev/helmr"
    const ref = payload.ref?.trim() || "main"
    assertRepository(repository)
    assertGitRef(ref)

    const checks: CheckResult[] = [
      await collectCheck("guide-mounted", () => checkCommand("guide-mounted", ["test", "-f", "/opt/helmr-dev-workflows/guides/INDEX.md"])),
      await collectCheck("nix-version", () => checkCommand("nix-version", ["nix", "--version"])),
      await collectCheck("git-version", () => checkCommand("git-version", ["git", "--version"])),
      await collectCheck("gh-version", () => checkCommand("gh-version", ["gh", "--version"])),
      await collectCheck("github-access", () => checkCommand("github-access", ["gh", "repo", "view", repository, "--json", "nameWithOwner,defaultBranchRef,isPrivate"], ghEnv())),
      await collectCheck("dev-runtime-mounts", () => checkCommand("dev-runtime-mounts", ["sh", "-ceu", "test -c /dev/tty && mountpoint -q /dev/pts && mountpoint -q /dev/shm && printf DEV_RUNTIME_OK"])),
      await collectCheck("namespace-unshare", () => checkCommand("namespace-unshare", ["sh", "-ceu", "unshare --mount true && unshare --uts true && unshare --ipc true && unshare --net true && unshare --pid --fork true && unshare --user true && printf NAMESPACE_OK"])),
      await collectCheck("nix-sandbox-config", () => checkCommand("nix-sandbox-config", ["sh", "-ceu", "nix show-config | grep -q '^sandbox = true$' && nix show-config | grep -q '^sandbox-fallback = false$' && printf NIX_SANDBOX_OK"])),
    ]

    const checkout = await collectCheck("github-checkout", () => ensureGitCheckout(repository, ref))
    checks.push(checkout)
    if (checkout.ok) {
      checks.push(await collectCheck("checkout-commit", () => checkCommand("checkout-commit", ["git", "rev-parse", "--verify", "HEAD^{commit}"])))
      checks.push(await collectCheck("checkout-status", () => checkCommand("checkout-status", ["git", "status", "--short"])))
      checks.push(await collectCheck("checkout-short-sha", () => checkCommand("checkout-short-sha", ["git", "rev-parse", "--short", "HEAD"])))
      checks.push(await collectCheck("nix-develop", () => checkCommand("nix-develop", ["nix", "develop", "--accept-flake-config", "-c", "sh", "-ceu", "command -v git >/dev/null && command -v rg >/dev/null && printf NIX_DEVELOP_OK"])))
    } else {
      checks.push(skippedCheck("checkout-commit", "github-checkout failed"))
      checks.push(skippedCheck("checkout-status", "github-checkout failed"))
      checks.push(skippedCheck("checkout-short-sha", "github-checkout failed"))
      checks.push(skippedCheck("nix-develop", "github-checkout failed"))
    }

    const sdk = {
      claude: await collectCheck("claude-sdk", () => runClaude(payload.claudeModel?.trim() || DEFAULT_CLAUDE_MODEL).then((marker) => sdkResult("claude-sdk", marker))),
      codex: await collectCheck("codex-sdk", () => runCodex(payload.codexModel?.trim() || DEFAULT_CODEX_MODEL).then((marker) => sdkResult("codex-sdk", marker))),
      cursor: await collectCheck("cursor-sdk", () => runCursor(payload.cursorModel?.trim() || DEFAULT_CURSOR_MODEL).then((marker) => sdkResult("cursor-sdk", marker))),
    }
    const failures = [...checks, ...Object.values(sdk)].filter((check) => !check.ok)
    const report = {
      ok: failures.length === 0,
      repository,
      ref,
      checks,
      sdk,
    }

    logger.info({
      phase: "agent-toolchain-smoke",
      repository,
      ref,
      commandChecks: checks.length,
      sdk,
    })
    await writeFile("agent-toolchain-smoke-report.json", `${JSON.stringify(report, null, 2)}\n`)
    if (failures.length > 0) {
      logger.error({ phase: "agent-toolchain-smoke", repository, ref, failures })
      throw new Error(`agent toolchain smoke failed ${failures.length} check(s): ${failureSummary(failures)}`)
    }
    return report
  },
})

async function runClaude(model: string): Promise<string> {
  const options: ClaudeOptions = {
    cwd: process.cwd(),
    model,
    permissionMode: "dontAsk",
    allowedTools: ["Read"],
    maxTurns: 3,
    env: {
      ...baseEnv(),
      ANTHROPIC_API_KEY: requiredEnv("ANTHROPIC_API_KEY"),
      CLAUDE_AGENT_SDK_CLIENT_APP: "helmr-dev-workflows/agent-toolchain-smoke",
    },
  }
  const stream = query({
    prompt: renderToolchainAgentPrompt("HELMR_CLAUDE_OK"),
    options,
  })

  for await (const message of stream) {
    if (message.type !== "result") continue
    if (message.subtype === "success") return assertMarker("claude", message.result, "HELMR_CLAUDE_OK")
    throw new Error(`Claude SDK check failed: ${message.errors.join("\n")}`)
  }
  throw new Error("Claude SDK check finished without a result")
}

async function runCodex(model: string): Promise<string> {
  const options: CodexThreadOptions = {
    model,
    sandboxMode: "read-only",
    approvalPolicy: "never",
    workingDirectory: process.cwd(),
    modelReasoningEffort: "low",
  }
  const output = await runCodexTurn(requiredEnv("OPENAI_API_KEY"), renderToolchainAgentPrompt("HELMR_CODEX_OK"), options)
  return assertMarker("codex", output, "HELMR_CODEX_OK")
}

async function runCursor(model: string): Promise<string> {
  const original = compactEnv(process.env)
  replaceProcessEnv({
    ...baseEnv(),
    CURSOR_API_KEY: requiredEnv("CURSOR_API_KEY"),
  })
  try {
    const agent = await Agent.create({
      apiKey: requiredEnv("CURSOR_API_KEY"),
      model: { id: model },
      local: { cwd: process.cwd() },
    })
    try {
      const run = await agent.send(renderToolchainAgentPrompt("HELMR_CURSOR_OK"), {
        model: { id: model },
        local: { force: true },
      })
      const result = await run.wait()
      if (result.status !== "finished") {
        throw new Error(`Cursor SDK check ended with status ${result.status}`)
      }
      return assertMarker("cursor", result.result ?? "", "HELMR_CURSOR_OK")
    } finally {
      agent.close()
    }
  } finally {
    replaceProcessEnv(original)
  }
}

function renderToolchainAgentPrompt(marker: string): string {
  return [
    "You are running the Helmr toolchain check.",
    "Do not modify files. Do not create branches, commits, pushes, issues, pull requests, or external side effects.",
    renderAgentGuideInstruction("agent toolchain smoke", ["nix-validation.md", "scope-security.md"]),
    "If accessible, read `/opt/helmr-dev-workflows/guides/INDEX.md` and `/opt/helmr-dev-workflows/guides/nix-validation.md`.",
    `Reply with ${marker} only.`,
  ].join("\n")
}

async function ensureGitCheckout(repository: string, ref: string): Promise<CheckResult> {
  const checkoutPath = `${process.cwd()}/.agent-toolchain-smoke-checkout`
  await runCommand(["rm", "-rf", checkoutPath])
  await runCommand(["git", "clone", "--filter=blob:none", "--no-checkout", `https://github.com/${repository}.git`, checkoutPath], ghEnv())
  process.chdir(checkoutPath)
  await runCommand(["git", "fetch", "--depth", "1", "origin", ref], ghEnv())
  await runCommand(["git", "checkout", "--detach", "FETCH_HEAD"])
  return {
    name: "github-checkout",
    ok: true,
    detail: {
      repository,
      ref,
      checkoutPath,
    },
  }
}

async function collectCheck(name: string, run: () => Promise<CheckResult>): Promise<CheckResult> {
  try {
    return await run()
  } catch (error) {
    return {
      name,
      ok: false,
      detail: error instanceof Error ? { message: error.message, name: error.name } : { message: String(error) },
    }
  }
}

function skippedCheck(name: string, reason: string): CheckResult {
  return {
    name,
    ok: false,
    detail: {
      skipped: true,
      reason,
    },
  }
}

function sdkResult(name: string, marker: string): CheckResult {
  return {
    name,
    ok: true,
    detail: {
      marker,
    },
  }
}

function failureSummary(failures: readonly CheckResult[]): string {
  return failures.map((failure) => {
    const message = typeof failure.detail === "object" && failure.detail !== null && "message" in failure.detail
      ? String((failure.detail as { readonly message?: unknown }).message)
      : JSON.stringify(failure.detail)
    return `${failure.name}: ${message.slice(0, 500)}`
  }).join("; ")
}

async function checkCommand(name: string, command: readonly string[], env: Record<string, string> = baseEnv()): Promise<CheckResult> {
  const output = await runCommand(command, env)
  return {
    name,
    ok: true,
    detail: {
      command: command.join(" "),
      output: output.trim().slice(0, 2000),
    },
  }
}

function ghEnv(): Record<string, string> {
  return {
    ...baseEnv(),
    GITHUB_TOKEN: requiredEnv("GITHUB_TOKEN"),
    GH_TOKEN: requiredEnv("GITHUB_TOKEN"),
  }
}

function baseEnv(): Record<string, string> {
  return baseAgentEnv()
}

function compactEnv(env: NodeJS.ProcessEnv): Record<string, string> {
  return Object.fromEntries(
    Object.entries(env).filter((entry): entry is [string, string] => typeof entry[1] === "string"),
  )
}

function replaceProcessEnv(env: Record<string, string>): void {
  for (const key of Object.keys(process.env)) {
    delete process.env[key]
  }
  Object.assign(process.env, env)
}

function requiredEnv(key: string): string {
  const value = process.env[key]
  if (!value) throw new Error(`${key} is required`)
  return value
}

function assertMarker(name: string, value: string, marker: string): string {
  const trimmed = value.trim()
  if (!trimmed.includes(marker)) {
    throw new Error(`${name} SDK check did not return ${marker}: ${trimmed.slice(0, 500)}`)
  }
  return marker
}

function assertRepository(value: string): void {
  if (!/^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/.test(value)) {
    throw new Error(`expected GitHub repository in owner/name form, received: ${value}`)
  }
}

function assertGitRef(value: string): void {
  if (!value || value.includes("\0") || value.startsWith("-")) {
    throw new Error(`expected a safe git ref, received: ${value}`)
  }
}
