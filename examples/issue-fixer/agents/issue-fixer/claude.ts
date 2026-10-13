import { agent } from "@helmr/sdk"
import { ClaudeHarness } from "./claude-harness"
import { checkRepository, issuePrompt, repository } from "./checks"
import { issueFixerComputer } from "./computer"

export const claudeIssueFixer = agent({
  id: "claude-issue-fixer",
  template: issueFixerComputer,
  async init(context) {
    const cwd = repository()
    const mcp = await context.mcp.getConnectionOptions()
    const harness = await ClaudeHarness.open(cwd, context.session.id, process.env, { helmr: { type: "http", url: mcp.url, headers: mcp.headers } })
    return { cwd, harness }
  },
  async turn(turn, { initResult }) {
    const result = await initResult.harness.run(turn, issuePrompt(turn.input))
    await checkRepository(initResult.cwd, turn.signal)
    return result
  },
})
