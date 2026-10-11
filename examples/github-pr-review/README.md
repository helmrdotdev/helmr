# GitHub PR Review

An Agent reads a GitHub pull request and returns a summary of changed files.
It does not post a review. GitHub requests carry the Turn's abort signal.

Create an Environment Secret for the GitHub token and replace the example UUID
in `tasks/review-pull-request.ts` with its returned stable Secret ID before
deployment. The Computer binds that Secret to `GITHUB_TOKEN` in raw mode so the
application can set the request authorization header. Keep the value out of
source and Turn input.

```bash
helmr secret create GITHUB_TOKEN --project PROJECT --env ENVIRONMENT --json
helmr deploy PATH/TO/github-pr-review --project PROJECT --env ENVIRONMENT
helmr agent start github-pr-review --text '{"owner":"OWNER","repo":"REPOSITORY","prNumber":123}' --project PROJECT --env ENVIRONMENT --json
```

The Agent parses the pull-request coordinates from JSON in the text.
The Secret command reads its value from standard input when no value argument
is supplied. Supply it through your existing secret-management workflow. Use the
admitted Session/Turn IDs to wait for the result with an explicit timeout.
