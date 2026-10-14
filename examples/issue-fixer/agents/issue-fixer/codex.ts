import { agent } from "@helmr/sdk"
import { CodexHarness } from "./codex-harness"
import { checkRepository, issuePrompt, repository } from "./checks"
import { issueFixerComputer } from "./computer"

export const codexIssueFixer = agent({
  id: "codex-issue-fixer",
  template: issueFixerComputer,
  async init(context) {
    const cwd = repository()
    const mcp = await context.mcp.getConnectionOptions()
    const harness = await CodexHarness.open(cwd, context.session.id, process.env, mcp)
    return { cwd, harness }
  },
  async turn(turn, { initResult }) {
    const { response, ...result } = await initResult.harness.run(turn, issuePrompt(turn.input))
    await checkRepository(initResult.cwd, turn.signal)
    if (response !== undefined) await turn.respond(response)
    return result
  },
})
