import { agent } from "@helmr/sdk"
import { ClaudeBridge } from "./claude-bridge"
import { checkRepository, issuePrompt, repository } from "./checks"
import { issueFixerComputer } from "./computer"

export const claudeIssueFixer = agent({
  id: "claude-issue-fixer",
  template: issueFixerComputer,
  async init(context) {
    const cwd = repository()
    const mcp = await context.mcp.getConnectionOptions()
    const bridge = await ClaudeBridge.open(cwd, context.session.id, process.env, { helmr: { type: "http", url: mcp.url, headers: mcp.headers } })
    return { cwd, bridge }
  },
  async turn(turn, { initResult }) {
    const { response, ...result } = await initResult.bridge.run(turn, issuePrompt(turn.input))
    await checkRepository(initResult.cwd, turn.signal)
    if (response !== undefined) await turn.respond(response)
    return result
  },
})
