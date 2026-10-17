import { readdir, readFile } from "node:fs/promises"
import { join } from "node:path"
import { query, type PermissionResult } from "@anthropic-ai/claude-agent-sdk"
import type { Turn } from "@helmr/sdk"

export interface ClaudeOptions {
  readonly cwd: string
  readonly sessionId: string
  readonly model: string
  readonly appendSystemPrompt: string
}

// Runs one Turn as one Claude Code prompt. Claude Code keeps the conversation
// in a transcript on the Computer's disk, which Helmr saves with the Session,
// so each Turn resumes where the previous one ended.
export async function runClaude(turn: Turn, options: ClaudeOptions): Promise<string | undefined> {
  const configDir = join(options.cwd, ".helmr", "claude")
  const resume = await hasTranscript(configDir, options.sessionId)
  turn.signal.throwIfAborted()
  const abortController = new AbortController()
  turn.signal.addEventListener("abort", () => abortController.abort(turn.signal.reason), { once: true })
  let response: string | undefined
  let failed = false
  const messages = query({
    prompt: turn.input.map(part => part.text).join(""),
    options: {
      cwd: options.cwd,
      model: options.model,
      systemPrompt: { type: "preset", preset: "claude_code", append: options.appendSystemPrompt },
      ...(resume ? { resume: options.sessionId } : { sessionId: options.sessionId }),
      settingSources: [],
      abortController,
      // Background commands would outlive the Turn, so Claude Code runs every
      // command in the foreground.
      env: { ...process.env, CLAUDE_CONFIG_DIR: configDir, CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC: "1", CLAUDE_CODE_DISABLE_BACKGROUND_TASKS: "1" },
      // Claude asks follow-up questions in its reply; the person answers in the thread.
      disallowedTools: ["AskUserQuestion"],
      canUseTool: (toolName, input, { signal, title }) => approve(turn, title ?? `Allow ${toolName}?`, input, signal),
    },
  })
  // Read to the end so Claude Code has exited before the Turn settles.
  for await (const message of messages) {
    if (message.type !== "result") continue
    if (message.subtype !== "success" || message.is_error) failed = true
    else response = message.result
  }
  if (failed) throw new Error("Claude Code could not finish this request")
  return response
}

async function hasTranscript(configDir: string, sessionId: string): Promise<boolean> {
  const projects = join(configDir, "projects")
  for (const directory of await readdir(projects).catch(missing([]))) {
    // A transcript without a user message cannot be resumed.
    const transcript = await readFile(join(projects, directory, `${sessionId}.jsonl`), "utf8").catch(missing(""))
    if (transcript.includes('"type":"user"')) return true
  }
  return false
}

function missing<T>(value: T) {
  return (error: NodeJS.ErrnoException): T => {
    if (error.code === "ENOENT") return value
    throw error
  }
}

// Tool use needs a person's approval in Slack or the Console.
async function approve(turn: Turn, title: string, input: Record<string, unknown>, signal: AbortSignal): Promise<PermissionResult> {
  const { answer } = await turn.ask({
    prompt: [{ type: "text", text: title }, { type: "json", value: JSON.parse(JSON.stringify(input)) }],
    type: "choice",
    options: [{ id: "allow", label: "Allow once", value: true }, { id: "deny", label: "Deny", value: false }],
  }, { signal })
  return answer.selected[0]?.value === true ? { behavior: "allow", updatedInput: input } : { behavior: "deny", message: "The person denied this" }
}
