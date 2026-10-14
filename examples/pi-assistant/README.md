# Pi prompt recipe

This minimal integration uses `@mariozechner/pi-coding-agent` **0.73.1**. It is a
recipe and fixture, not a deployable Agent project. Prepare the local SDK packages
with `nix develop -c ./scripts/build-npm-packages.sh`, then run `bun install`,
`bun run typecheck`, and `bun run test` here in the pinned Nix environment.

Application turn code calls `runPi(turn, text, openSession, nativeUI)`. The factory
returns a fresh `AgentSession` created with the native `createAgentSession` API.
The application supplies its native UI implementation and configures model,
authentication, resources, and tools. After `runPi` returns, the application may
rewrite or omit `response`, or call `await turn.respond(response)` once. Its own
return value remains the machine result.

The recipe registers lifecycle ownership before opening the session, installs one
session event listener, and disposes the session after its owned prompt. Do not
share that instance with another prompt caller. Native tools and extensions must
join their work and honor abort; arbitrary detached subprocesses are not contained
by this in-process recipe. Configure contained tools before deployment. This
recipe does not create a general permission policy or imply that a dialog grant
allows unrelated tools.

Completed assistant text before another assistant cycle or tool execution becomes
output. Reasoning, tool arguments, and tool results stay native. Text is bounded at
256 KiB per message, 8 MiB and 16,384 messages per prompt. The final candidate is
held until the owned `prompt()` resolves (including native retries), its final
assistant event is observed, and its state is neither error nor aborted. An
intermediate `turn_end` or `agent_end` is insufficient. The pin has no
`agent_settled` event. Native failure may publish verified held explanation as
output; cancellation never publishes late text or a final response.

The UI wrapper translates select, input, and confirm to scoped Helmr questions.
An explicit No is a normal answer. Cancelled input/select returns `undefined`;
cancelled confirmation returns `false`, while `cancelledDialogs` records the
cancellation separately. Other native UI methods remain application-owned. Live
steering and queued follow-ups are intentionally left to a separate application
integration; this single-prompt recipe does not claim to have applied them.

Tests use synthetic events and compile against the exact native package. They
prove bridge behavior, not a live provider run or a deployed runtime.
