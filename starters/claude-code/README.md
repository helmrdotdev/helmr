# Claude Code Agent

A conversational Helmr Agent that runs Claude Code through the Claude Agent SDK.
Each Turn runs one Claude Code prompt that resumes the Session's conversation,
so follow-ups in the same Slack thread remember earlier messages. Tool use
waits for a person's approval in Slack or the Console.

| File | Contents |
| --- | --- |
| `agents/settings.ts` | The Agent name, its introduction, the model and the model key binding. |
| `agents/<name>.ts` | The Template (image, resources, Secret binding) and the Agent. |
| `agents/claude-bridge.ts` | Runs a Turn with Claude Code and asks for tool approvals. |

Before deploying, add the `ANTHROPIC_API_KEY` Secret in the Console for the
target Environment. The key is only sent to `https://api.anthropic.com`; the
Computer sees a placeholder.

```sh
bun install
helmr deploy --project PROJECT --env ENVIRONMENT
```
