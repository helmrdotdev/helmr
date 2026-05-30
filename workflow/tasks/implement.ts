import { cache, image, sandbox, source, task } from "@helmr/sdk"
import {
  runClaude,
  runClaudeSlashCommand,
  runClaudeWithOperatorQuestions,
} from "./integrations/claude"
import {
  runCodex,
  runCodexJson,
  runCodexReview,
  runCodexWithOperatorQuestions,
  triageSchema,
} from "./integrations/codex"
import { runCursor } from "./integrations/cursor"
import type { OperatorQuestionRequest } from "./integrations/questions"
import {
  artifactPath,
  artifacts,
  renderFeatureDesign,
  renderOperatorQuestions,
  renderReviewLoop,
  writeJson,
  writeMarkdown,
} from "./implement/artifacts"
import { createOrFindPullRequest, resolvePullRequestBase } from "./integrations/github"
import { readAuthSecrets } from "./implement/payload"
import {
  renderClaudePlanPrompt,
  renderCodexPlanPrompt,
  renderCodexReviewInstructions,
  renderCodexSecurityTriagePrompt,
  renderCodexTriagePrompt,
  renderCursorFixPrompt,
  renderCursorExplorationPrompt,
  renderCursorImplementationPrompt,
  renderCursorSecurityFixPrompt,
} from "./implement/prompts"
import {
  assertCleanSnapshot,
  assertCleanWorkspace,
  assertCurrentBranch,
  assertHeadContainsBase,
  assertHeadEqualsBase,
  commitChanges,
  currentBranch,
  exposeUntrackedFilesToReview,
  prepareGitWorkspace,
  pushBranch,
  repoSnapshot,
  workingTreeDiff,
} from "./integrations/git"
import { CLAUDE_PLAN_MAX_TURNS, CLAUDE_REVIEW_MAX_TURNS } from "./models"
import { normalizePayload, requireGitHubSource, type Payload } from "./integrations/types"
import type { OperatorQuestionRecord, ReviewRound, TriageResult } from "./integrations/types"

const dependencyInputs = source.directory(".", {
  ignore: ["*", "!package.json", "!bun.lock", "!tsconfig.json"],
})
const guideInputs = source.directory("guides")

const base = image("helmr-implementation-workflow")
  .from("node:24-bookworm-slim")
  .workdir("/workspace")
  .copy("/workspace", dependencyInputs)
  .copy("/opt/helmr-workflow/guides", guideInputs)
  .run([
    "sh",
    "-ceu",
    [
      "apt-get update",
      "apt-get install -y --no-install-recommends ca-certificates curl xz-utils git ripgrep python3 make g++",
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
    cache: [{ mountPath: "/root/.bun/install/cache", cache: cache("implementation-workflow-bun") }],
  })

const sbx = sandbox("helmr-implementation-workflow")
  .image(base)
  .resources({ cpu: 2, memory: "4Gi" })

const claudeReviewTools = [
  "Read",
  "Glob",
  "Grep",
  "LS",
  "Bash(git diff:*)",
  "Bash(git status:*)",
  "Bash(git ls-files:*)",
  "Bash(git show:*)",
  "Bash(git rev-parse:*)",
  "Bash(git merge-base:*)",
  "Bash(rg:*)",
  "Bash(sed:*)",
  "Bash(cat:*)",
  "Bash(find:*)",
  "Bash(wc:*)",
]

export const implement = task({
  id: "implement",
  sandbox: sbx,
  maxDuration: 43200,
  secrets: {
    ANTHROPIC_API_KEY: { env: "ANTHROPIC_API_KEY" },
    OPENAI_API_KEY: { env: "OPENAI_API_KEY" },
    CURSOR_API_KEY: { env: "CURSOR_API_KEY" },
    GITHUB_TOKEN: { env: "GITHUB_TOKEN" },
  },
  run: async (payload: Payload, ctx) => {
    const input = normalizePayload(payload)
    const auth = readAuthSecrets()
    const source = requireGitHubSource(ctx)

    await prepareGitWorkspace(ctx, auth.githubToken)
    const prBaseBranch = resolvePullRequestBase(source, input.prBaseBranch)
    const repo = await repoSnapshot(source.resolvedSha)
    assertCleanSnapshot(repo, "implementation workflow")
    const rounds: ReviewRound[] = []
    const operatorQuestions: OperatorQuestionRecord[] = []
    const askOperator = async (request: OperatorQuestionRequest): Promise<string> => {
      const reply = await ctx.wait.token<{ text: string }>({
        displayText: renderOperatorPrompt(request),
        timeout: input.operatorInputTimeout,
      })
      const record = {
        phase: request.phase,
        questionNumber: request.questionNumber,
        question: request.question,
        context: request.context,
        answer: reply.text,
        at: new Date().toISOString(),
      }
      operatorQuestions.push(record)
      await writeMarkdown("operator-questions.md", renderOperatorQuestions(operatorQuestions))
      ctx.log.info({ phase: "operator-input", agentPhase: request.phase, questionNumber: request.questionNumber })
      return reply.text
    }

    await writeMarkdown(
      "00-feature-design.md",
      renderFeatureDesign(input, repo, source, ctx.run.id, prBaseBranch),
    )
    ctx.log.info({ phase: "brief", artifact: artifactPath("00-feature-design.md") })

    const exploration = await runCursor(
      input,
      auth.cursorApiKey,
      renderCursorExplorationPrompt(input, repo),
    )
    await assertCleanWorkspace("exploration phase")
    await assertCurrentBranch(repo.branch, "exploration phase")
    await writeMarkdown("01-exploration.md", exploration)
    ctx.log.info({ phase: "exploration", artifact: artifactPath("01-exploration.md") })

    await writeMarkdown("operator-questions.md", renderOperatorQuestions(operatorQuestions))

    const claudePlan = await runClaudeWithOperatorQuestions(
      input,
      "claude-plan",
      renderClaudePlanPrompt(input, repo, exploration),
      {
        cwd: process.cwd(),
        model: input.claudeModel,
        permissionMode: "dontAsk",
        allowedTools: ["Read", "Glob", "Grep", "LS"],
        maxTurns: CLAUDE_PLAN_MAX_TURNS,
      },
      askOperator,
    )
    await writeMarkdown("02-claude-plan.md", claudePlan)
    ctx.log.info({ phase: "claude-plan", artifact: artifactPath("02-claude-plan.md") })

    const codexPlan = await runCodexWithOperatorQuestions(
      input,
      auth.openaiApiKey,
      "codex-plan-review",
      renderCodexPlanPrompt(input, exploration, claudePlan),
      {
        model: input.codexModel,
        sandboxMode: "read-only",
        approvalPolicy: "never",
        workingDirectory: process.cwd(),
        skipGitRepoCheck: true,
        modelReasoningEffort: "high",
      },
      askOperator,
    )
    await writeMarkdown("03-codex-plan-review.md", codexPlan)
    ctx.log.info({ phase: "codex-plan-review", artifact: artifactPath("03-codex-plan-review.md") })

    const cursorImplementation = await runCursor(
      input,
      auth.cursorApiKey,
      renderCursorImplementationPrompt(input, exploration, claudePlan, codexPlan),
    )
    await writeMarkdown("04-cursor-implementation.md", cursorImplementation)
    ctx.log.info({ phase: "cursor-implementation", artifact: artifactPath("04-cursor-implementation.md") })
    const headBranch = await currentBranch({ previousBranch: repo.branch })
    await assertHeadEqualsBase(repo.baseSha, "implementation phase")
    ctx.log.info({ phase: "branch", headBranch })

    const agentReports: AgentReport[] = [{ phase: "cursor-implementation", content: cursorImplementation }]
    let finalFindingCount = Number.POSITIVE_INFINITY
    for (let round = 1; round <= input.maxReviewRounds; round += 1) {
      await assertHeadEqualsBase(repo.baseSha, `review round ${round}`)
      await exposeUntrackedFilesToReview()
      const diff = await workingTreeDiff(repo.baseSha)
      const reviewContext = renderReviewContext(agentReports)
      ctx.log.info({ phase: "review-start", round })
      const [codexReview, claudeCodeReview] = await Promise.all([
        (async () => {
          ctx.log.info({ phase: "codex-review-start", round })
          const review = await runCodexReview(
            auth.openaiApiKey,
            renderCodexReviewInstructions(input, round, reviewContext),
            diff,
            {
              model: input.codexModel,
              sandboxMode: "read-only",
              approvalPolicy: "never",
              workingDirectory: process.cwd(),
              skipGitRepoCheck: true,
              modelReasoningEffort: "high",
            },
          )
          ctx.log.info({ phase: "codex-review-complete", round })
          return review
        })(),
        (async () => {
          ctx.log.info({ phase: "claude-code-review-start", round, maxTurns: CLAUDE_REVIEW_MAX_TURNS })
          const review = await runClaudeSlashCommand(
            `claude-code-review-${round}`,
            "code-review",
            ["high"],
            {
              cwd: process.cwd(),
              model: input.claudeModel,
              permissionMode: "dontAsk",
              allowedTools: claudeReviewTools,
              maxTurns: CLAUDE_REVIEW_MAX_TURNS,
            },
          )
          ctx.log.info({ phase: "claude-code-review-complete", round })
          return review
        })(),
      ])
      ctx.log.info({ phase: "review-complete", round })
      const codexTriage = await runCodexJson<TriageResult>(
        auth.openaiApiKey,
        renderCodexTriagePrompt(input, round, codexReview, claudeCodeReview, reviewContext),
        triageSchema,
        {
          model: input.codexModel,
          sandboxMode: "read-only",
          approvalPolicy: "never",
          workingDirectory: process.cwd(),
          skipGitRepoCheck: true,
          modelReasoningEffort: "high",
        },
      )

      finalFindingCount = codexTriage.findings.length
      ctx.log.info({ phase: "triage", round, findings: finalFindingCount })

      if (finalFindingCount === 0) {
        rounds.push({ round, codexReview, claudeCodeReview, codexTriage })
        break
      }

      ctx.log.info({ phase: "cursor-fix-start", round, findings: finalFindingCount })
      const cursorFix = await runCursor(
        input,
        auth.cursorApiKey,
        renderCursorFixPrompt(input, round, codexTriage),
      )
      agentReports.push({ phase: `cursor-fix-round-${round}`, content: cursorFix })
      ctx.log.info({ phase: "cursor-fix-complete", round })
      rounds.push({ round, codexReview, claudeCodeReview, codexTriage, cursorFix })
      await assertCurrentBranch(headBranch, `fix round ${round}`)
      await assertHeadEqualsBase(repo.baseSha, `fix round ${round}`)
      await writeMarkdown(`05-round-${round}-fix.md`, cursorFix)
    }

    await writeMarkdown("05-review-loop.md", renderReviewLoop(rounds))
    if (finalFindingCount !== 0) {
      await writeMarkdown("06-security-review.md", "Not run because the review loop ended with actionable findings.")
      await writeMarkdown("07-security-fix.md", "Not run because the final security review did not run.")
      const result = {
        status: "blocked",
        reason: "review loop ended before Codex triage reached zero findings",
        runId: ctx.run.id,
        repository: source.repository,
        headBranch,
        rounds,
        operatorQuestions,
        artifacts: artifacts(),
      }
      await writeJson("implementation-result.json", result)
      return result
    }

    await assertCurrentBranch(headBranch, "security review phase")
    await assertHeadEqualsBase(repo.baseSha, "security review phase")
    await exposeUntrackedFilesToReview()
    ctx.log.info({ phase: "security-review-start", maxTurns: CLAUDE_REVIEW_MAX_TURNS })
    const securityReview = await runClaudeSlashCommand(
      "security-review",
      "security-review",
      [],
      {
        cwd: process.cwd(),
        model: input.claudeModel,
        permissionMode: "dontAsk",
        allowedTools: claudeReviewTools,
        maxTurns: CLAUDE_REVIEW_MAX_TURNS,
      },
    )
    await writeMarkdown("06-security-review.md", securityReview)
    ctx.log.info({ phase: "security-review-complete", artifact: artifactPath("06-security-review.md") })

    const securityTriage = await runCodexJson<TriageResult>(
      auth.openaiApiKey,
      renderCodexSecurityTriagePrompt(input, securityReview, renderReviewContext(agentReports)),
      triageSchema,
      {
        model: input.codexModel,
        sandboxMode: "read-only",
        approvalPolicy: "never",
        workingDirectory: process.cwd(),
        skipGitRepoCheck: true,
        modelReasoningEffort: "high",
      },
    )
    ctx.log.info({ phase: "security-triage", findings: securityTriage.findings.length })
    if (securityTriage.findings.length > 0) {
      ctx.log.info({ phase: "security-fix-start", findings: securityTriage.findings.length })
      const securityFix = await runCursor(
        input,
        auth.cursorApiKey,
        renderCursorSecurityFixPrompt(input, securityTriage),
      )
      agentReports.push({ phase: "cursor-security-fix", content: securityFix })
      await assertCurrentBranch(headBranch, "security fix phase")
      await assertHeadEqualsBase(repo.baseSha, "security fix phase")
      await writeMarkdown("07-security-fix.md", securityFix)
      ctx.log.info({ phase: "security-fix-complete", artifact: artifactPath("07-security-fix.md") })
    } else {
      await writeMarkdown("07-security-fix.md", "No actionable security findings.")
    }

    await assertCurrentBranch(headBranch, "commit phase")
    await assertHeadEqualsBase(repo.baseSha, "commit phase")
    await commitChanges(input)
    await assertCurrentBranch(headBranch, "push phase")
    await assertHeadContainsBase(repo.baseSha, "push phase")
    await pushBranch(source.repository, headBranch, auth.githubToken)
    const pullRequest = await createOrFindPullRequest(auth.githubToken, source, input, headBranch)

    const result = {
      status: "pr-created",
      runId: ctx.run.id,
      repository: source.repository,
      headBranch,
      prUrl: pullRequest.html_url,
      prNumber: pullRequest.number,
      rounds,
      securityReview,
      securityTriage,
      operatorQuestions,
      artifacts: artifacts(),
    }
    await writeJson("implementation-result.json", result)
    return result
  },
})

function renderOperatorPrompt(request: OperatorQuestionRequest): string {
  return [
    `Agent phase: ${request.phase}`,
    `Question ${request.questionNumber}:`,
    "",
    request.question,
    "",
    request.context ? "Context:" : "",
    request.context ? request.context : "",
    "",
    "Reply with the information needed to continue this workflow. Do not include secret values.",
  ].filter((line) => line !== "").join("\n")
}

interface AgentReport {
  readonly phase: string
  readonly content: string
}

function renderReviewContext(reports: readonly AgentReport[]): string {
  const maxReportChars = 8000
  const maxReports = 6
  const selectedReports = reports.length <= maxReports
    ? reports
    : [reports[0], ...reports.slice(-(maxReports - 1))]
  const omittedCount = reports.length - selectedReports.length
  const sections = selectedReports.map((report) => [
    `## ${report.phase}`,
    "",
    report.content.length > maxReportChars
      ? `${report.content.slice(0, maxReportChars)}\n\n... report truncated ...`
      : report.content,
  ].join("\n"))
  if (omittedCount > 0) {
    sections.splice(1, 0, `## Omitted reports\n\n${omittedCount} older fix report(s) omitted to keep review context bounded.`)
  }
  return sections.join("\n\n")
}
