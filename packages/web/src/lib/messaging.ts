export const positioning =
  "Devin, Cursor Cloud and Codex Cloud ship the agent as a finished product. Helmr ships it as infrastructure: every part is a TypeScript value you pick, swap, or write yourself.";

export const logos = {
  claude: "/logos/claude.svg",
  openai: "/logos/openai.svg",
  cursor: "/logos/cursor.svg",
  opencode: "/logos/opencode.svg",
  pi: "/logos/pi.svg",
  slack: "/logos/slack.svg",
  discord: "/logos/discord.svg",
  microsoftTeams: "/logos/microsoft-teams.svg",
  github: "/logos/github.svg",
  linear: "/logos/linear.svg",
} as const;

export const usecases = [
  {
    key: "review",
    name: "PR reviewer",
    icon: "merge",
    id: "review-pr",
    primitive: "task",
    imports: 'import { image, sandbox, source, task, tokens } from "@helmr/sdk"',
    head: `export const reviewPr = task({
  id: "review-pr",
  payload: z.object({ prNumber: z.number().int().positive() }),
  run: async (event, ctx) => {
    const brief = \`Review PR #\${event.prNumber} and propose a patch.\``,
    meta: '      metadata: { subject: "Post this review to GitHub?" }',
    action: "    if (decision.approved) await postReview(event.prNumber, output)",
  },
  {
    key: "bugs",
    name: "Bug hunter",
    icon: "bug",
    id: "bug-hunter",
    primitive: "task",
    imports: 'import { image, sandbox, source, task, tokens } from "@helmr/sdk"',
    head: `export const huntBug = task({
  id: "bug-hunter",
  payload: z.object({ errorId: z.string() }),
  run: async (event, ctx) => {
    const brief = \`Reproduce error \${event.errorId} in /workspace, find the root cause and write a fix.\``,
    meta: '      metadata: { subject: "Post the root cause and fix?" }',
    action: "    if (decision.approved) await postReport(event.errorId, output)",
  },
  {
    key: "mention",
    name: "@-mention teammate",
    icon: "mention",
    id: "fix-issue",
    primitive: "actor",
    imports: 'import { actor, image, sandbox, source, tokens } from "@helmr/sdk"',
    head: `export const fixIssue = actor({
  id: "fix-issue",
  async run(session) {
    const turn = await session.receive({ idleTimeout: "30m" })
    if (turn === null) return
    const event = z.object({
    issue: z.string(), repo: z.string(),
    channel: z.string().optional(), channelId: z.string().optional(),
    conversationId: z.string().optional(), prNumber: z.number().optional(),
    issueId: z.string().optional()
  }).parse(turn.input)
    const brief = \`Fix \${event.issue} in \${event.repo}. Run the tests, then open a PR.\``,
    meta: '      metadata: { subject: "Open a PR for this fix?" }',
    action: `    if (decision.approved) {
      await turn.output.write({
        type: "permission_admitted", requestId: approval.id,
        actionBinding: { action: "open_pr", repo: event.repo, output }
      })
      await openPr(event.repo, output)
      await turn.output.write({ type: "pr-opened" })
    }
    await turn.complete()`,
  },
  {
    key: "fleet",
    name: "3 a.m. fleet job",
    icon: "moon",
    id: "fleet-bump",
    primitive: "schedules.task",
    imports: 'import { image, sandbox, schedules, source, tokens } from "@helmr/sdk"',
    head: `export const fleetBump = schedules.task({
  id: "fleet-bump",
  cron: { pattern: "0 3 * * *", timezone: "UTC" },
  run: async (event, ctx) => {
    const brief = \`Bump dependencies across every repo; run each test suite.\``,
    meta: '      metadata: { subject: "Open PRs for all repos?" }',
    action: "    if (decision.approved) await openFleetPrs(output)",
  },
] as const;

export const harnesses = [
  {
    key: "claude",
    name: "Claude Agent SDK",
    meta: "@anthropic-ai/claude-agent-sdk",
    icon: logos.claude,
    imports: 'import { query } from "@anthropic-ai/claude-agent-sdk"',
    agent: `    let output = ""
    for await (const message of query({
      prompt: brief,
      options: {
        cwd: "/workspace",
        permissionMode: "bypassPermissions", // the microVM is the sandbox
        allowDangerouslySkipPermissions: true
      }
    })) {
      if (message.type === "result" && message.subtype === "success") output = message.result
    }`,
  },
  {
    key: "codex",
    name: "Codex",
    meta: "@openai/codex-sdk",
    icon: logos.openai,
    imports: 'import { Codex } from "@openai/codex-sdk"',
    agent: `    const codex = new Codex()
    const thread = codex.startThread({
      workingDirectory: "/workspace",
      sandboxMode: "danger-full-access" // the microVM is the sandbox
    })
    const nativeTurn = await thread.run(brief)
    const output = nativeTurn.finalResponse`,
  },
  {
    key: "cursor",
    name: "Cursor",
    meta: "@cursor/sdk",
    icon: logos.cursor,
    imports: 'import { Agent } from "@cursor/sdk"',
    agent: `    const result = await Agent.prompt(brief, {
      apiKey: process.env.CURSOR_API_KEY!, // protected env: the VM only holds a placeholder
      model: { id: "composer-2.5" },
      local: { cwd: "/workspace" }
    })
    const output = result.result ?? ""`,
  },
  {
    key: "opencode",
    name: "OpenCode",
    meta: "@opencode-ai/sdk · opencode serve",
    icon: logos.opencode,
    imports: 'import { createOpencode } from "@opencode-ai/sdk"',
    agent: `    const { client } = await createOpencode()
    const nativeSession = await client.session.create({ body: { title: "run" } })
    const result = await client.session.prompt({
      path: { id: nativeSession.data.id },
      body: {
        model: { providerID: "openrouter", modelID: "z-ai/glm-4.6" },
        parts: [{ type: "text", text: brief }]
      }
    })
    const output = result.data.parts
      .filter((part) => part.type === "text")
      .map((part) => part.text)
      .join("")`,
  },
  {
    key: "pi",
    name: "Pi",
    meta: "@earendil-works/pi-coding-agent",
    icon: logos.pi,
    imports: 'import { createAgentSession, SessionManager } from "@earendil-works/pi-coding-agent"',
    agent: `    const { session: nativeSession } = await createAgentSession({
      cwd: "/workspace",
      sessionManager: SessionManager.inMemory()
    })
    let output = ""
    nativeSession.subscribe((e) => {
      if (e.type === "message_update" && e.assistantMessageEvent.type === "text_delta") {
        output += e.assistantMessageEvent.delta
      }
    })
    await nativeSession.prompt(brief)`,
  },
  {
    key: "own",
    name: "Your own harness",
    meta: "ai · @ai-sdk/amazon-bedrock",
    icon: null,
    imports: `import { generateText, tool, stepCountIs } from "ai"
import { bedrock } from "@ai-sdk/amazon-bedrock"`,
    agent: `    const { text: output } = await generateText({
      model: bedrock("anthropic.claude-sonnet-4-6"), // or Vertex, Ollama — any provider the AI SDK speaks
      tools: {
        sh: tool({
          description: "Run a shell command in /workspace",
          inputSchema: z.object({ cmd: z.string() }),
          execute: ({ cmd }) => sh(cmd)
        })
      },
      stopWhen: stepCountIs(40),
      prompt: brief
    })`,
  },
] as const;

export const interfaces = [
  {
    key: "slack",
    name: "Slack",
    icon: logos.slack,
    code: `    await sendSlackApproval({
      channel: event.channel,
      callbackUrl: approval.callbackUrl
    })`,
  },
  {
    key: "discord",
    name: "Discord",
    icon: logos.discord,
    code: `    await sendDiscordApproval({
      channelId: event.channelId,
      callbackUrl: approval.callbackUrl
    })`,
  },
  {
    key: "teams",
    name: "Microsoft Teams",
    icon: logos.microsoftTeams,
    code: `    await sendTeamsCard({
      conversationId: event.conversationId,
      callbackUrl: approval.callbackUrl
    })`,
  },
  {
    key: "github",
    name: "GitHub",
    icon: logos.github,
    code: `    await commentOnPr({
      prNumber: event.prNumber,
      body: approval.callbackUrl
    })`,
  },
  {
    key: "linear",
    name: "Linear",
    icon: logos.linear,
    code: `    await commentOnIssue({
      issueId: event.issueId,
      body: approval.callbackUrl
    })`,
  },
  {
    key: "product",
    name: "Your product",
    icon: null,
    code: `    await notify({
      topic: "approval-requested",
      callbackUrl: approval.callbackUrl
    })`,
  },
] as const;
