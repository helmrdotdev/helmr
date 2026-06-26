# GitHub PR Review

Read a GitHub pull request, summarize the changed files, and post the summary
only after a dashboard approval.

```bash
helmr secret set GITHUB_TOKEN "ghp_..."
helmr deploy PATH/TO/github-pr-review

helmr session start github-pr-review \
  --payload-json '{"owner":"OWNER","repo":"REPO","prNumber":123}'
```
