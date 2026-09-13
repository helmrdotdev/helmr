# Helmr

**Build your own software factory.**

Infrastructure and APIs for your own agent harness.  
Your agents. Your workflows. Your rules.

Write your agent logic in TypeScript, bring your tools and integrations, and
run it in isolated Linux microVMs. Keep your workspace across runs, pause for
human input, and inspect what happened.

## What you get

- **Persistent workspaces** — files and dependencies kept across runs.
- **Tasks and sessions** — one-shot jobs or agents you can steer over time.
- **Human input** — pause for approval or external input, then continue.
- **Secrets and visibility** — runtime secret injection, logs, and run history.
- **Infrastructure you control** — self-host in your own AWS account.

## Get started

Install the CLI:

```sh
curl -fsSL https://helmr.dev/install | bash
```

Follow the [quickstart](https://helmr.dev/docs/quickstart/) to deploy and run your
first task. You'll need a running control plane and worker; the
[self-hosting guide](https://helmr.dev/docs/self-hosting/overview/) covers setup.

## Explore

- [Documentation](https://helmr.dev/docs/)
- [TypeScript SDK](https://helmr.dev/docs/reference/sdk/overview/)
- [REST API](https://helmr.dev/docs/reference/rest-api/overview/)
- [Examples](examples/)

Early, active development. APIs and deployment details may change before a
stable release.

[Apache 2.0](LICENSE).
