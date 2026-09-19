# Hello World

The smallest Helmr Task and Workspace declarations: build an image, define the
Workspace that uses it, accept Task payload, and write a file during the Run.

```bash
helmr deploy PATH/TO/hello-world --project PROJECT --env ENVIRONMENT
```

## Session lifecycle examples

`tasks/session.ts` adds two editable Actors using the same Workspace:

- `checked-reply` emits finite output, performs a deterministic check, then
  completes with a required typed result or fails. Enqueue
  `{"text":"hello","expected":"hello"}` for success; change `expected` to observe
  output followed by `turn.failed`. A stream ending never determines success.
- `external-ci` emits an authenticated integration request and waits on a generic
  Token. Enqueue `{"commit":"your-commit"}`; consume `ci_requested` from the Session
  event timeline and deduplicate the CI start using that event's ID. Complete the
  Token through an authenticated client:

  ```ts
  await client.tokens.complete(tokenId, {
    result: { passed: true, reportUrl: "https://ci.example/build/123" },
    idempotencyKey: "ci:build-123:finished",
  })
  ```

Start an Actor without input, then enqueue with the returned Session ID. Read
`helmr actor events SESSION_ID --json` and retain `next_after`; inspect a Turn
with `helmr actor turn get SESSION_ID TURN_ID`. Output and lifecycle share one
sequence. The external CI service is application-owned; the example does not
contact a provider automatically.

To exercise a managed wait, enqueue A and wait for `ci_requested`, then enqueue B.
Interrupt A using its exact Turn ID. A Token completion that races interruption
does not authorize further Turn output or replay A; inspect the Turn's terminal
outcome. Queued B remains behind the hold. Once interruption converges, resume
using the exact hold from the stop receipt (`actor resume SESSION_ID --hold ID`).
Stopping this wait does not globally cancel its Token or another consumer's wait.
The SDK supports shared Token consumers; this example creates one per Turn.

This managed Token wait can checkpoint; it makes no claim that an arbitrary
provider socket or callback survives suspension. Native VM race/restore
qualification is separate from typechecking these examples.
