import { agent } from "@helmr/sdk"
import { createRuntimeMcpConnection } from "@helmr/sdk/mcp"
import { ClaudeHarness } from "./claude-harness"
import { checkRepository, issuePrompt, repository } from "./checks"
import { issueFixerComputer } from "./computer"

export const claudeIssueFixer = agent({
  id: "claude-issue-fixer",
  computer: issueFixerComputer,
  async setup(context) {
    const cwd = repository()
    const mcp = await createRuntimeMcpConnection()
    const harness = await ClaudeHarness.open(cwd, context.session.id, process.env, { helmr: { type: "http", url: mcp.url, headers: mcp.headers } })
    return { cwd, harness }
  },
  async turn(turn, { setupResult }) {
    const result = await setupResult.harness.run(turn, issuePrompt(turn.input))
    await checkRepository(setupResult.cwd, turn.signal)
    return result
  },
})
