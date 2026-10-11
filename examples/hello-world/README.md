# Hello World

Define an Agent and its Computer, validate each Turn's input, and write a file
on the Computer. The Session retains that file across later Turns.

```bash
helmr deploy PATH/TO/hello-world --project PROJECT --env ENVIRONMENT
helmr agent start hello-world --text Helmr --project PROJECT --env ENVIRONMENT --json
```

The admission includes a Session ID and a Turn ID. Inspect the outcome with
`helmr session turn wait SESSION_ID TURN_ID --timeout 5m --json`, using the same
project and environment flags. A timeout stops observation and leaves work running.

## Output, response and questions

`tasks/session.ts` defines `checked-reply` with the same default Computer definition:

- `checked-reply` writes output, validates it, stages a response, and returns a
  machine result. Send `{"text":"hello","expected":"hello"}` as the text of one
  input part for success.
  Change `expected` to observe output followed by failure. Output alone never
  establishes success; successful settlement publishes the staged response.

This Agent parses JSON from text. For example:

```bash
helmr agent start checked-reply --text '{"text":"hello","expected":"hello"}' --project PROJECT --env ENVIRONMENT --json
```

Use `helmr session events SESSION_ID --json` and retain `next_after` to reconnect.
To hold a Session and its owned descendants, use `helmr session interrupt SESSION_ID`.
Queued work remains; resume the owning Session using the returned exact hold ID
with `helmr session resume SESSION_ID --hold HOLD_ID`. Interrupted input is not
replayed. A question response racing interruption does not authorize continued
output or successful settlement of the interrupted Turn.

These examples require a deployed environment for lifecycle and restoration
qualification; local typechecking does not establish that runtime evidence.
