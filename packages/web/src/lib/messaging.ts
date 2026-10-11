export const positioning =
  "Choose your native harness, tools and workflow in TypeScript. Helmr gives that code Sessions, attributable Turns and persistent Computers in infrastructure you control.";

export const logos = {
  claude: "/logos/claude.svg",
  openai: "/logos/openai.svg",
  slack: "/logos/slack.svg",
} as const;

export const usecases = ([
  { key: "review", name: "PR reviewer", icon: "merge", id: "review-pr", brief: "Review the requested PR. Explain findings; do not publish a review." },
  { key: "bugs", name: "Bug hunter", icon: "bug", id: "bug-hunter", brief: "Reproduce the reported error, find its cause and test a fix." },
  { key: "mention", name: "Conversation teammate", icon: "mention", id: "repo-teammate", brief: "Help with the repository request. Ask when a decision is needed." },
  { key: "fleet", name: "3 a.m. audit", icon: "moon", id: "nightly-audit", brief: "Audit this repository for outdated dependencies and report findings." },
] as const).map(usecase => ({
  ...usecase,
  primitive: usecase.key === "fleet" ? "triggers.cron()" : "agent()",
  imports: `import { agent${usecase.key === "fleet" ? ", triggers" : ""} } from "@helmr/sdk"`,
  head: `export const workflow = agent({
  id: "${usecase.id}",
  computer: workspace,
  ${usecase.key === "fleet" ? `
  closeAfterIdle: "5m",
  triggers: [triggers.cron("nightly", "0 3 * * *", {
    timezone: "UTC", input: [{ type: "text", text: "Audit dependencies." }]
  })],` : ""}`,
  prompt: `    const brief = ${JSON.stringify(usecase.brief)}
      + "\\nInput: " + JSON.stringify(turn.input)`,
}));

// These imports refer to application code in the linked issue-fixer example.
// Helmr does not supply a common provider adapter API.
export const harnesses = [
  {
    key: "claude", name: "Claude Agent SDK", meta: "application adapter · Claude Agent SDK", icon: logos.claude,
    imports: `import { ClaudeHarness } from "./claude-harness"
import { createRuntimeMcpConnection } from "@helmr/sdk/mcp"`,
    turn: "  async turn(turn, { setupResult }) {",
    setup: `  async setup(context) {
    const mcp = await createRuntimeMcpConnection()
    return ClaudeHarness.open("/workspace/repository", context.session.id,
      process.env, { helmr: { type: "http", url: mcp.url, headers: mcp.headers } })
  },`,
    agent: "    return setupResult.run(turn, brief)",
  },
  {
    key: "codex", name: "Codex", meta: "application adapter · Codex", icon: logos.openai,
    imports: `import { CodexHarness } from "./codex-harness"
import { createRuntimeMcpConnection } from "@helmr/sdk/mcp"`,
    turn: "  async turn(turn, { setupResult }) {",
    setup: `  async setup(context) {
    const mcp = await createRuntimeMcpConnection()
    return CodexHarness.open("/workspace/repository", context.session.id,
      process.env, mcp)
  },`,
    agent: "    return setupResult.run(turn, brief)",
  },
  {
    key: "own", name: "Your own harness", meta: "application code · implement runMyAgent", icon: null,
    imports: 'import { runMyAgent } from "./my-agent"',
    turn: "  async turn(turn) {",
    setup: "  // runMyAgent owns native lifecycle, cancellation and output handling.",
    agent: "    return runMyAgent(turn, brief)",
  },
] as const;

export const interfaces = [
  {
    key: "slack", name: "Slack", icon: logos.slack,
    imports: 'import type { HelmrClient, InputContent } from "@helmr/sdk"',
    code: `// Call from your authenticated application, after deployment.
// Connect the Agent to its dedicated Slack app and invite it to this channel.
// channelId is the actual Slack channel ID, such as C0123456789.
export async function start(client: HelmrClient, input: InputContent, channelId: string) {
  return client.agents.start(workflow.id, {
    input, slack: { channelId }
  })
}`,
  },
  {
    key: "cli", name: "CLI", icon: null,
    imports: "",
    code: `// After deployment, from an authenticated shell:
// helmr agent start AGENT_ID --project agents --env development \\
//   --input-json '[{"type":"text","text":"Review the latest changes."}]'
// Inspect questions with: helmr session turn ask list SESSION_ID TURN_ID
// Include --project agents --env development for each command.`,
  },
  {
    key: "product", name: "Your product", icon: null,
    imports: 'import type { HelmrClient, InputContent } from "@helmr/sdk"',
    code: `// Call from your authenticated server, after deployment.
export async function start(client: HelmrClient, input: InputContent) {
  const { session, turn } = await client.agents.start(workflow.id, { input })
  // Follow session.events and answer exact Turn asks in your UI.
  return { sessionId: session.id, turnId: turn.id }
}`,
  },
] as const;

export const fixedCode = {
  computer: 'import { issueFixerComputer as workspace } from "./computer"',
  close: "  }\n})",
} as const;

export const codeRecipe: ReadonlyArray<readonly [string, "usecase" | "agent" | "interface" | null]> = [
  ["usecase.imports", "usecase"],
  ["agent.imports", "agent"],
  ["interface.imports", "interface"],
  ["fixed.computer", null],
  ["blank", null],
  ["usecase.head", "usecase"],
  ["agent.setup", "agent"],
  ["agent.turn", "agent"],
  ["usecase.prompt", "usecase"],
  ["agent.body", "agent"],
  ["fixed.close", null],
  ["blank", null],
  ["interface.lines", "interface"],
];

export function composeExample(usecase: typeof usecases[number], harness: typeof harnesses[number], transport: typeof interfaces[number]): string {
  const parts: Record<string, string> = {
    "usecase.imports": usecase.imports, "usecase.head": usecase.head, "usecase.prompt": usecase.prompt,
    "agent.imports": harness.imports, "agent.setup": harness.setup, "agent.body": harness.agent, "agent.turn": harness.turn,
    "interface.lines": transport.code, "interface.imports": transport.imports, blank: "",
    ...Object.fromEntries(Object.entries(fixedCode).map(([key, value]) => [`fixed.${key}`, value])),
  };
  return codeRecipe.map(([key]) => parts[key]).join("\n");
}
