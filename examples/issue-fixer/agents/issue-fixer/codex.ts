import { agent } from "@helmr/sdk"
import { createRuntimeMcpConnection } from "@helmr/sdk/mcp"
import { CodexHarness } from "./codex-harness"
import { checkRepository, issuePrompt, repository } from "./checks"
import { issueFixerComputer } from "./computer"

export const codexIssueFixer = agent({
  id: "codex-issue-fixer",
  computer: issueFixerComputer,
  async init(context) {
    const cwd = repository()
    const mcp = await createRuntimeMcpConnection()
    const harness = await CodexHarness.open(cwd, context.session.id, process.env, mcp)
    return { cwd, harness }
  },
  async turn(turn, { initResult }) {
    const result = await initResult.harness.run(turn, issuePrompt(turn.input))
    await checkRepository(initResult.cwd, turn.signal)
    return result
  },
})
