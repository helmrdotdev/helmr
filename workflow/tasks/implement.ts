import { cache, image, sandbox, source, task } from "@helmr/sdk"
import {
  runClaude,
  runClaudeWithOperatorQuestions,
  runCodex,
  runCodexJson,
  runCodexWithOperatorQuestions,
  runCursor,
  triageSchema,
  type OperatorQuestionRequest,
} from "./implement/agents"
import {
  artifactPath,
  artifacts,
  renderFeatureDesign,
  renderOperatorQuestions,
  renderReviewLoop,
  writeJson,
  writeMarkdown,
} from "./implement/artifacts"
import { createOrFindPullRequest } from "./implement/github"
import { readAuthSecrets } from "./implement/payload"
import {
  renderClaudePlanPrompt,
  renderClaudeReviewPrompt,
  renderCodexPlanPrompt,
  renderCodexReviewPrompt,
  renderCodexTriagePrompt,
  renderCursorFixPrompt,
  renderCursorExplorationPrompt,
  renderCursorImplementationPrompt,
} from "./implement/prompts"
import {
  assertCleanSnapshot,
  assertCleanWorkspace,
  assertCurrentBranch,
  assertHeadContainsBase,
  assertHeadEqualsBase,
  commitChanges,
  currentBranch,
  prepareGitWorkspace,
  pushBranch,
  repoSnapshot,
  workingTreeDiff,
} from "./implement/repo"
import { normalizePayload, type Payload } from "./implement/types"
import type { OperatorQuestionRecord, ReviewRound, TriageResult } from "./implement/types"

const dependencyInputs = source.directory(".", {
  ignore: ["*", "!package.json", "!bun.lock", "!tsconfig.json"],
})

const base = image("helmr-implementation-workflow")
  .from("oven/bun:1.3.10-debian")
  .workdir("/workspace")
  .copy("/workspace", dependencyInputs)
  .run([
    "sh",
    "-ceu",
    [
      "apt-get update",
      "apt-get install -y --no-install-recommends git ca-certificates",
      "rm -rf /var/lib/apt/lists/*",
    ].join(" && "),
  ])
  .run(["bun", "install", "--frozen-lockfile", "--ignore-scripts"], {
    cache: [{ mountPath: "/root/.bun/install/cache", cache: cache("implementation-workflow-bun") }],
  })

const sbx = sandbox("helmr-implementation-workflow")
  .image(base)
  .resources({ cpu: 2, memory: "4Gi" })

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

    const repository = await prepareGitWorkspace(input, auth.githubToken)
    const repo = await repoSnapshot()
    assertCleanSnapshot(repo, "implementation workflow")
    const rounds: ReviewRound[] = []
    const operatorQuestions: OperatorQuestionRecord[] = []
    const askOperator = async (request: OperatorQuestionRequest): Promise<string> => {
      const reply = await ctx.wait.message(renderOperatorPrompt(request), {
        timeout: input.operatorInputTimeout,
      })
      const record = {
        phase: request.phase,
        questionNumber: request.questionNumber,
        question: request.question,
        context: request.context,
        answer: reply.text,
        answeredBy: reply.sentBy,
        at: reply.at.toISOString(),
      }
      operatorQuestions.push(record)
      await writeMarkdown("operator-questions.md", renderOperatorQuestions(operatorQuestions))
      ctx.log.info({ phase: "operator-input", agentPhase: request.phase, questionNumber: request.questionNumber })
      return reply.text
    }

    await writeMarkdown("00-feature-design.md", renderFeatureDesign(input, repo, repository, ctx.run.id))
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
        maxTurns: 8,
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

    let finalFindingCount = Number.POSITIVE_INFINITY
    for (let round = 1; round <= input.maxReviewRounds; round += 1) {
      await assertHeadEqualsBase(repo.baseSha, `review round ${round}`)
      const diff = await workingTreeDiff(repo.baseSha)
      const codexReview = await runCodex(
        auth.openaiApiKey,
        renderCodexReviewPrompt(input, round, diff),
        {
          model: input.codexModel,
          sandboxMode: "read-only",
          approvalPolicy: "never",
          workingDirectory: process.cwd(),
          skipGitRepoCheck: true,
          modelReasoningEffort: "high",
        },
      )
      const claudeReview = await runClaude(
        `claude-review-${round}`,
        renderClaudeReviewPrompt(input, round, diff),
        {
          cwd: process.cwd(),
          model: input.claudeModel,
          permissionMode: "dontAsk",
          allowedTools: ["Read", "Glob", "Grep", "LS"],
          maxTurns: 8,
        },
      )
      const codexTriage = await runCodexJson<TriageResult>(
        auth.openaiApiKey,
        renderCodexTriagePrompt(input, round, codexReview, claudeReview),
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
        rounds.push({ round, codexReview, claudeReview, codexTriage })
        break
      }

      const cursorFix = await runCursor(
        input,
        auth.cursorApiKey,
        renderCursorFixPrompt(input, round, codexTriage),
      )
      rounds.push({ round, codexReview, claudeReview, codexTriage, cursorFix })
      await assertCurrentBranch(headBranch, `fix round ${round}`)
      await assertHeadEqualsBase(repo.baseSha, `fix round ${round}`)
      await writeMarkdown(`05-round-${round}-fix.md`, cursorFix)
    }

    await writeMarkdown("05-review-loop.md", renderReviewLoop(rounds))
    if (finalFindingCount !== 0) {
      const result = {
        status: "blocked",
        reason: "review loop ended before Codex triage reached zero findings",
        runId: ctx.run.id,
        repository,
        headBranch,
        rounds,
        operatorQuestions,
        artifacts: artifacts(),
      }
      await writeJson("implementation-result.json", result)
      return result
    }

    await assertCurrentBranch(headBranch, "commit phase")
    await assertHeadEqualsBase(repo.baseSha, "commit phase")
    await commitChanges(input)
    await assertCurrentBranch(headBranch, "push phase")
    await assertHeadContainsBase(repo.baseSha, "push phase")
    await pushBranch(repository, headBranch, auth.githubToken)
    const pullRequest = await createOrFindPullRequest(auth.githubToken, repository, input, headBranch)

    const result = {
      status: "pr-created",
      runId: ctx.run.id,
      repository,
      headBranch,
      prUrl: pullRequest.html_url,
      prNumber: pullRequest.number,
      rounds,
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
