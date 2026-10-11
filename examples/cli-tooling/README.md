# CLI Tooling

Install `ripgrep` in a Computer image and call it from an Agent Turn. The handler
creates `sample.ts`, searches it, and writes `cli-tooling-report.json` on the
Computer. The report includes its Turn ID. The child process receives the Turn's
abort signal.

```bash
helmr deploy PATH/TO/cli-tooling --project PROJECT --env ENVIRONMENT
helmr agent start cli-tooling --text '{"pattern":"export const"}' --project PROJECT --env ENVIRONMENT --json
```

The Agent parses its options from JSON in the text. Use the returned Session and Turn IDs with
`helmr session turn wait SESSION_ID TURN_ID --timeout 5m --json` and the same scope
flags. The report is the machine result after successful settlement.
