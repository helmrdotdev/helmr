import { image, sandbox, task } from "@helmr/sdk"

const base = image("helmr-implementation-workflow")
  .from("debian:trixie-slim")
  .run([
    "sh",
    "-ceu",
    "apt-get update && apt-get install -y --no-install-recommends git ca-certificates && rm -rf /var/lib/apt/lists/*",
  ])

const sbx = sandbox("helmr-implementation-workflow")
  .image(base)
  .resources({ cpu: 1, memory: "1Gi" })

interface Payload {
  readonly goal?: string
  readonly issue?: string
  readonly targetBranch?: string
  readonly baseBranch?: string
  readonly prTitle?: string
  readonly maxReviewRounds?: number
}

interface PhaseRecord {
  readonly phase: string
  readonly author: string
  readonly text: string
}

interface ReviewRound {
  readonly round: number
  readonly codexReview: string
  readonly claudeReview: string
  readonly codexTriage: string
  readonly findingCount: number
  readonly composerFix?: string
}

export const implement = task({
  id: "implement",
  sandbox: sbx,
  maxDuration: 7200,
  run: async (payload: Payload, ctx) => {
    const input = normalizePayload(payload)
    const repo = await repoSnapshot()
    const records: PhaseRecord[] = []
    const rounds: ReviewRound[] = []

    const brief = renderBrief(input, repo, ctx.run.id)
    await writeMarkdown("00-implementation-brief.md", brief)
    ctx.log.info({ phase: "brief", path: "00-implementation-brief.md" })

    const claudePlan = await message(
      ctx,
      [
        "Paste Claude's implementation plan.",
        "It should cover scope, files, risks, validation, and open questions.",
      ].join("\n"),
    )
    records.push({ phase: "plan", author: "claude", text: claudePlan })

    const codexPlan = await message(
      ctx,
      [
        "Paste Codex's review or revision of Claude's plan.",
        "It should either approve the plan or name concrete changes before implementation.",
      ].join("\n"),
    )
    records.push({ phase: "plan-review", author: "codex", text: codexPlan })

    await writeMarkdown("01-plan.md", renderPlan(input, claudePlan, codexPlan))
    const planDecision = await ctx.wait.approval(
      [
        "Approve this plan and continue to Cursor Composer 2.5 implementation?",
        "",
        "The plan has been written to 01-plan.md.",
      ].join("\n"),
    )
    if (!planDecision.approved) {
      const result = {
        status: "plan-denied",
        runId: ctx.run.id,
        approvedBy: planDecision.approvedBy,
        artifacts: ["00-implementation-brief.md", "01-plan.md"],
      }
      await writeJson("implementation-result.json", result)
      return result
    }

    const composerImplementation = await message(
      ctx,
      [
        "Paste Cursor Composer 2.5's implementation summary.",
        "Include changed files, commands run, tests, and known gaps.",
      ].join("\n"),
    )
    records.push({ phase: "implementation", author: "cursor-composer-2.5", text: composerImplementation })
    await writeMarkdown("02-implementation.md", renderImplementation(composerImplementation))

    let finalFindingCount = Number.POSITIVE_INFINITY
    for (let round = 1; round <= input.maxReviewRounds; round++) {
      const codexReview = await message(
        ctx,
        `Round ${round}: paste Codex review findings. Use "none" if there are no actionable findings.`,
      )
      const claudeReview = await message(
        ctx,
        `Round ${round}: paste Claude review findings. Use "none" if there are no actionable findings.`,
      )
      const codexTriage = await message(
        ctx,
        [
          `Round ${round}: paste Codex triage across both reviews.`,
          "Include a line exactly like `findings: N` where N is the actionable finding count.",
        ].join("\n"),
      )
      const findingCount = parseFindingCount(codexTriage, [codexReview, claudeReview])
      finalFindingCount = findingCount

      if (findingCount === 0) {
        rounds.push({ round, codexReview, claudeReview, codexTriage, findingCount })
        ctx.log.info({ phase: "review", round, findings: 0 })
        break
      }

      const composerFix = await message(
        ctx,
        [
          `Round ${round}: Codex triaged ${findingCount} actionable finding(s).`,
          "Paste Cursor Composer 2.5's fix summary, including changed files and validation.",
        ].join("\n"),
      )
      rounds.push({ round, codexReview, claudeReview, codexTriage, findingCount, composerFix })
      ctx.log.info({ phase: "fix", round, findings: findingCount })
    }

    await writeMarkdown("03-review-loop.md", renderReviewLoop(rounds))

    if (finalFindingCount !== 0) {
      const result = {
        status: "blocked",
        reason: "review loop ended before Codex triage reached zero findings",
        runId: ctx.run.id,
        rounds,
        artifacts: [
          "00-implementation-brief.md",
          "01-plan.md",
          "02-implementation.md",
          "03-review-loop.md",
          "implementation-result.json",
        ],
      }
      await writeJson("implementation-result.json", result)
      return result
    }

    const prDecision = await ctx.wait.approval(
      [
        "Codex triage reported zero findings.",
        "Create the GitHub pull request now, then approve this waitpoint after the PR exists.",
      ].join("\n"),
    )
    if (!prDecision.approved) {
      const result = {
        status: "ready-no-pr",
        runId: ctx.run.id,
        approvedBy: prDecision.approvedBy,
        artifacts: ["00-implementation-brief.md", "01-plan.md", "02-implementation.md", "03-review-loop.md"],
      }
      await writeJson("implementation-result.json", result)
      return result
    }

    const prUrl = await message(ctx, "Paste the GitHub PR URL.")
    const result = {
      status: "pr-created",
      runId: ctx.run.id,
      prUrl: prUrl.trim(),
      records,
      rounds,
      artifacts: [
        "00-implementation-brief.md",
        "01-plan.md",
        "02-implementation.md",
        "03-review-loop.md",
        "implementation-result.json",
      ],
    }
    await writeJson("implementation-result.json", result)
    return result
  },
})

function normalizePayload(payload: Payload) {
  const goal = payload.goal?.trim()
  if (!goal) {
    throw new Error("payload.goal is required")
  }
  return {
    goal,
    issue: payload.issue?.trim() || "",
    targetBranch: payload.targetBranch?.trim() || "",
    baseBranch: payload.baseBranch?.trim() || "main",
    prTitle: payload.prTitle?.trim() || goal,
    maxReviewRounds: clampInteger(payload.maxReviewRounds ?? 3, 1, 10),
  }
}

function clampInteger(value: number, min: number, max: number): number {
  if (!Number.isInteger(value)) {
    throw new Error("payload.maxReviewRounds must be an integer")
  }
  return Math.min(Math.max(value, min), max)
}

async function message(ctx: { wait: { message: (prompt: string) => Promise<{ text: string }> } }, prompt: string): Promise<string> {
  const reply = await ctx.wait.message(prompt)
  return reply.text.trim()
}

async function repoSnapshot() {
  const [head, branch, status] = await Promise.all([
    run(["git", "rev-parse", "--short", "HEAD"]),
    run(["git", "branch", "--show-current"]),
    run(["git", "status", "--short"]),
  ])
  return {
    head: head.trim(),
    branch: branch.trim() || "detached",
    status: status.trim(),
  }
}

async function run(command: string[]): Promise<string> {
  const proc = Bun.spawn(command, { stdout: "pipe", stderr: "pipe" })
  const [stdout, stderr, exitCode] = await Promise.all([
    new Response(proc.stdout).text(),
    new Response(proc.stderr).text(),
    proc.exited,
  ])
  if (exitCode !== 0) {
    throw new Error(`${command.join(" ")} exited ${exitCode}: ${stderr}`)
  }
  return stdout
}

function parseFindingCount(triage: string, reviews: string[]): number {
  const explicit = triage.match(/(?:^|\n)\s*findings\s*:\s*(\d+)\s*(?:\n|$)/i)
  if (explicit) {
    return Number.parseInt(explicit[1] ?? "0", 10)
  }

  const texts = [triage, ...reviews]
  if (texts.every(isNoFindingText)) {
    return 0
  }

  const candidateLines = triage
    .split("\n")
    .map((line) => line.trim())
    .filter((line) => /^[-*]\s+/.test(line) || /^\d+[.)]\s+/.test(line))
  return Math.max(candidateLines.length, 1)
}

function isNoFindingText(value: string): boolean {
  const normalized = value.trim().toLowerCase()
  return /^(none|no findings|no actionable findings|findings\s*:\s*0|0 findings)[.!]?$/.test(normalized)
}

function renderBrief(input: ReturnType<typeof normalizePayload>, repo: Awaited<ReturnType<typeof repoSnapshot>>, runId: string): string {
  return [
    "# Implementation Brief",
    "",
    `Run: ${runId}`,
    `Goal: ${input.goal}`,
    input.issue ? `Issue: ${input.issue}` : "",
    `Base branch: ${input.baseBranch}`,
    input.targetBranch ? `Target branch: ${input.targetBranch}` : "",
    `PR title: ${input.prTitle}`,
    `Review rounds: ${input.maxReviewRounds}`,
    "",
    "## Workspace",
    "",
    `Branch: ${repo.branch}`,
    `HEAD: ${repo.head}`,
    "",
    "## Initial Status",
    "",
    "```text",
    repo.status || "clean",
    "```",
    "",
  ].filter(Boolean).join("\n")
}

function renderPlan(input: ReturnType<typeof normalizePayload>, claudePlan: string, codexPlan: string): string {
  return [
    "# Plan",
    "",
    `Goal: ${input.goal}`,
    "",
    "## Claude Plan",
    "",
    claudePlan,
    "",
    "## Codex Plan Review",
    "",
    codexPlan,
    "",
  ].join("\n")
}

function renderImplementation(summary: string): string {
  return ["# Implementation", "", "## Cursor Composer 2.5 Summary", "", summary, ""].join("\n")
}

function renderReviewLoop(rounds: ReviewRound[]): string {
  const sections = rounds.map((round) => [
    `## Round ${round.round}`,
    "",
    "### Codex Review",
    "",
    round.codexReview,
    "",
    "### Claude Review",
    "",
    round.claudeReview,
    "",
    "### Codex Triage",
    "",
    round.codexTriage,
    "",
    `Actionable findings: ${round.findingCount}`,
    "",
    round.composerFix ? "### Cursor Composer 2.5 Fix" : "",
    round.composerFix ? "" : "",
    round.composerFix ?? "",
    "",
  ].filter((line) => line !== "").join("\n"))

  return ["# Review Loop", "", ...sections, ""].join("\n")
}

async function writeMarkdown(path: string, value: string): Promise<void> {
  await Bun.write(path, value.endsWith("\n") ? value : `${value}\n`)
}

async function writeJson(path: string, value: unknown): Promise<void> {
  await Bun.write(path, `${JSON.stringify(value, null, 2)}\n`)
}
