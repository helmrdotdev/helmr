# Release security

Release workflows are treated as privileged code because they can publish files, container images, signed boot artifacts, and future AWS worker AMIs.

## Required repository settings

- Create a GitHub Actions environment named `release-production`.
- Restrict deployments to release tags or `main` workflow dispatch runs.
- For a single-maintainer project, leave required reviewers disabled so the maintainer can publish releases.
- When more than one maintainer can approve releases, require reviewer approval for `release-production` and disable self-approval.

## Workflow rules

- Do not use `pull_request_target`.
- Do not use GitHub Actions cache in release workflows.
- Do not pass `CACHIX_AUTH_TOKEN` to release workflows.
- Keep write credentials in the smallest possible job.
- Build jobs should use `contents: read` and upload workflow artifacts.
- Publish jobs should download artifacts and publish them without checking out or building repository code.
- `id-token: write` is only allowed when the line is marked with `security-check: allow-id-token` and the job is protected by the `release-production` environment.

## Worker AMI release rules

When the worker AMI release job is added, the GitHub job should only assume a narrowly scoped AWS role and start or monitor AWS Image Builder. The actual worker image build should happen inside AWS Image Builder with a separate least-privilege instance profile.
