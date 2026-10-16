import { agent } from "@helmr/sdk"
import { CodexBridge } from "./codex-bridge"
import { checkRepository, issuePrompt, repository } from "./checks"
import { issueFixerComputer } from "./computer"

export const codexIssueFixer = agent({
  id: "codex-issue-fixer",
  template: issueFixerComputer,
  async init(context) {
    const cwd = repository()
    const mcp = await context.mcp.getConnectionOptions()
    const bridge = await CodexBridge.open(cwd, context.session.id, process.env, mcp)
    return { cwd, bridge }
  },
  async turn(turn, { initResult }) {
    const { response, ...result } = await initResult.bridge.run(turn, issuePrompt(turn.input))
    await checkRepository(initResult.cwd, turn.signal)
    if (response !== undefined) await turn.respond(response)
    return result
  },
})
