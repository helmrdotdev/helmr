# GitHub PR Review

Read a GitHub pull request, summarize the changed files, and post the summary
only after a dashboard approval.

```bash
helmr secret set github-token "ghp_..."
helmr deploy PATH/TO/github-pr-review

helmr run github-pr-review \
  --repo OWNER/REPO \
  --ref main \
  --subpath PATH/TO/github-pr-review \
  --payload-json '{"owner":"OWNER","repo":"REPO","prNumber":123}' \
  --secret GITHUB_TOKEN=vault:github-token
```
