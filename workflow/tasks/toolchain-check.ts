import { query, type Options as ClaudeOptions } from "@anthropic-ai/claude-agent-sdk"
import { Agent } from "@cursor/sdk"
import { cache, image, sandbox, source, task } from "@helmr/sdk"
import { spawn } from "node:child_process"
import { runCodex as runCodexTurn, type CodexThreadOptions } from "./implement/agents"
import { renderAgentGuideInstruction } from "./implement/prompts"
import { DEFAULT_CLAUDE_MODEL, DEFAULT_CODEX_MODEL, DEFAULT_CURSOR_MODEL } from "./models"

const dependencyInputs = source.directory(".", {
  ignore: ["*", "!package.json", "!bun.lock", "!tsconfig.json"],
})
const guideInputs = source.directory("guides")

const base = image("helmr-toolchain-check")
  .from("node:24-bookworm-slim")
  .workdir("/workspace")
  .copy("/workspace", dependencyInputs)
  .copy("/opt/helmr-workflow/guides", guideInputs)
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
    cache: [{ mountPath: "/root/.bun/install/cache", cache: cache("toolchain-check-bun") }],
  })

const sbx = sandbox("helmr-toolchain-check")
  .image(base)
  .resources({ cpu: 2, memory: "4Gi" })

interface Payload {
  readonly repository?: string
  readonly ref?: string
  readonly claudeModel?: string
  readonly codexModel?: string
  readonly cursorModel?: string
}

interface CheckResult {
  readonly command: string
  readonly ok: true
  readonly output: string
}

export const toolchainCheck = task({
  id: "toolchain-check",
  sandbox: sbx,
  maxDuration: 1800,
  secrets: {
    ANTHROPIC_API_KEY: { env: "ANTHROPIC_API_KEY" },
    OPENAI_API_KEY: { env: "OPENAI_API_KEY" },
    CURSOR_API_KEY: { env: "CURSOR_API_KEY" },
    GITHUB_TOKEN: { env: "GITHUB_TOKEN" },
  },
  run: async (payload: Payload, ctx) => {
    const repository = payload.repository?.trim() || "helmrdotdev/helmr"
    const ref = payload.ref?.trim() || "main"
    assertRepository(repository)
    assertGitRef(ref)

    const checks: CheckResult[] = []
    checks.push(await checkCommand(["test", "-f", "/opt/helmr-workflow/guides/INDEX.md"]))
    checks.push(await checkCommand(["nix", "--version"]))
    checks.push(await checkCommand(["git", "--version"]))
    checks.push(await checkCommand(["gh", "--version"]))
    checks.push(await checkCommand(["gh", "repo", "view", repository, "--json", "nameWithOwner,defaultBranchRef,isPrivate"], ghEnv()))
    checks.push(await checkCommand(["git", "ls-remote", `https://github.com/${repository}.git`, ref], ghEnv()))

    await ensureGitCheckout(repository, ref)
    checks.push(await checkCommand(["git", "status", "--short"]))
    checks.push(await checkCommand(["git", "rev-parse", "--short", "HEAD"]))
    checks.push(await checkCommand(["sh", "-ceu", "test -c /dev/tty && mountpoint -q /dev/pts && mountpoint -q /dev/shm && printf DEV_RUNTIME_OK"]))
    checks.push(await checkCommand(["sh", "-ceu", "unshare --mount true && unshare --uts true && unshare --ipc true && unshare --net true && unshare --pid --fork true && unshare --user true && printf NAMESPACE_OK"]))
    checks.push(await checkCommand(["sh", "-ceu", "nix show-config | grep -q '^sandbox = true$' && nix show-config | grep -q '^sandbox-fallback = false$' && printf NIX_SANDBOX_OK"]))
    checks.push(await checkCommand(["nix", "develop", "--accept-flake-config", "-c", "sh", "-ceu", "command -v git >/dev/null && command -v rg >/dev/null && printf NIX_DEVELOP_OK"]))

    const sdk = {
      claude: await runClaude(payload.claudeModel?.trim() || DEFAULT_CLAUDE_MODEL),
      codex: await runCodex(payload.codexModel?.trim() || DEFAULT_CODEX_MODEL),
      cursor: await runCursor(payload.cursorModel?.trim() || DEFAULT_CURSOR_MODEL),
    }

    ctx.log.info({
      phase: "toolchain-check",
      repository,
      ref,
      commandChecks: checks.length,
      sdk,
    })

    return {
      repository,
      ref,
      pullRequestCreation: "disabled",
      checks,
      sdk,
    }
  },
})

async function runClaude(model: string): Promise<string> {
  const options: ClaudeOptions = {
    cwd: process.cwd(),
    model,
    permissionMode: "dontAsk",
    allowedTools: ["Read"],
    maxTurns: 1,
    env: {
      ...baseEnv(),
      ANTHROPIC_API_KEY: requiredEnv("ANTHROPIC_API_KEY"),
      CLAUDE_AGENT_SDK_CLIENT_APP: "helmr-workflow/toolchain-check",
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
    skipGitRepoCheck: true,
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
    renderAgentGuideInstruction("toolchain check", ["nix-validation.md", "scope-security.md"]),
    "If accessible, read `/opt/helmr-workflow/guides/INDEX.md` and `/opt/helmr-workflow/guides/nix-validation.md`.",
    `Reply with ${marker} only.`,
  ].join("\n")
}

async function ensureGitCheckout(repository: string, ref: string): Promise<void> {
  if (await isGitWorkspace()) return
  const checkoutPath = `${process.cwd()}/.helmr-toolchain-checkout`
  await checkCommand(["git", "clone", "--depth", "1", "--branch", ref, `https://github.com/${repository}.git`, checkoutPath], ghEnv())
  process.chdir(checkoutPath)
}

async function isGitWorkspace(): Promise<boolean> {
  try {
    await checkCommand(["git", "rev-parse", "--is-inside-work-tree"])
    return true
  } catch {
    return false
  }
}

async function checkCommand(command: readonly string[], env: Record<string, string> = baseEnv()): Promise<CheckResult> {
  const output = await run(command, env)
  return {
    command: command.join(" "),
    ok: true,
    output: output.trim().slice(0, 2000),
  }
}

function run(command: readonly string[], env: Record<string, string>): Promise<string> {
  return new Promise((resolve, reject) => {
    const proc = spawn(command[0] ?? "", command.slice(1), {
      env,
      stdio: ["ignore", "pipe", "pipe"],
    })
    let stdout = ""
    let stderr = ""
    proc.stdout.setEncoding("utf8")
    proc.stderr.setEncoding("utf8")
    proc.stdout.on("data", (chunk: string) => {
      stdout += chunk
    })
    proc.stderr.on("data", (chunk: string) => {
      stderr += chunk
    })
    proc.on("error", reject)
    proc.on("close", (exitCode) => {
      if (exitCode !== 0) {
        reject(new Error(`${command.join(" ")} exited ${exitCode}: ${stderr.trim()}`))
        return
      }
      resolve(stdout || stderr)
    })
  })
}

function ghEnv(): Record<string, string> {
  return {
    ...baseEnv(),
    GITHUB_TOKEN: requiredEnv("GITHUB_TOKEN"),
    GH_TOKEN: requiredEnv("GITHUB_TOKEN"),
  }
}

function baseEnv(): Record<string, string> {
  const env: Record<string, string> = {}
  for (const key of ["HOME", "PATH", "TMPDIR", "USER", "LOGNAME", "LANG", "LC_ALL"]) {
    const value = process.env[key]
    if (typeof value === "string") env[key] = value
  }
  return env
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
