# Claude Code Agent

A conversational Helmr Agent that runs Claude Code through the Claude Agent SDK.
Each Helmr Session keeps one Claude conversation, so follow-ups in the same
Slack thread remember earlier messages. Tool use and Claude's questions become
Helmr questions that people answer in Slack or the Console.

| File | Contents |
| --- | --- |
| `agents/settings.ts` | The Agent name, its introduction, the model and the model key binding. |
| `agents/<name>.ts` | The Template (image, resources, Secret binding) and the Agent. |
| `agents/claude-bridge.ts` | Connects Claude Code to Helmr Sessions and Turns. |
| `agents/native-questions.ts` | Turns Claude's questions and tool approvals into Helmr questions. |

Before deploying, add the `ANTHROPIC_API_KEY` Secret in the Console for the
target Environment. The key is only sent to `https://api.anthropic.com`; the
Computer sees a placeholder.

```sh
bun install
helmr deploy --project PROJECT --env ENVIRONMENT
```
