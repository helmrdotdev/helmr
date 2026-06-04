# Release security

Release workflows are treated as privileged code because they can publish files, container images, signed boot artifacts, and future AWS worker AMIs.

## Required repository settings

- Create a GitHub Actions environment named `release-production`.
- Restrict deployments to release tags or `main` workflow dispatch runs.
- For a single-maintainer project, leave required reviewers disabled so the maintainer can publish releases.
- When more than one maintainer can approve releases, require reviewer approval for `release-production` and disable self-approval.
- Treat prerelease tags such as `vX.Y.Z-rc.N` as release jobs: they still publish public
  artifacts, use the same protected environment, and must be marked as prereleases instead of
  latest releases.
- Add environment variables for official worker AMI releases:
  - `RELEASE_AWS_ROLE_ARN`: IAM role assumed by GitHub OIDC.
  - `RELEASE_AWS_STATE_BUCKET`: S3 backend bucket for the worker-image OpenTofu state.
  - `RELEASE_AWS_STATE_KEY`: S3 backend key for the release worker-image stack,
    initially `helmr/stacks/release-worker-image/terraform.tfstate`.
  - `RELEASE_AWS_REGION`: primary Image Builder region, initially `us-east-1`.
  - `RELEASE_AWS_STATE_REGION`: state bucket region, if different from `RELEASE_AWS_REGION`.
  - `RELEASE_WORKER_AMI_REGIONS`: comma-separated public AMI regions, initially `us-east-1,us-west-2,ap-northeast-1`.
  - `RELEASE_WORKER_AMI_KEEP`: public release AMIs to keep per region before building the next
    release AMI, initially `4` so the default AWS public AMI quota has one free slot.

## Workflow rules

- Do not use `pull_request_target`.
- Do not use GitHub Actions cache in release workflows.
- Do not pass `CACHIX_AUTH_TOKEN` to release workflows.
- Keep write credentials in the smallest possible job.
- Build jobs should use `contents: read` and upload workflow artifacts.
- Publish jobs should download artifacts, check out only the release helper scripts needed for
  manifest verification, and avoid building repository code.
- `id-token: write` is only allowed when the line is marked with `security-check: allow-id-token` and the job is protected by the `release-production` environment.

## Worker AMI release rules

The worker AMI release job assumes a narrowly scoped AWS role through GitHub OIDC and starts or
monitors AWS Image Builder. The publish job assumes the same role to verify that every AMI recorded
in `aws-artifacts.json` is visible in its declared region before publishing the manifest. Do not add
long-lived AWS access keys to GitHub. The actual worker image build happens inside AWS Image
Builder with a separate least-privilege instance profile.

Scope the role trust policy to the repository and `release-production` environment, with
`token.actions.githubusercontent.com:aud` equal to `sts.amazonaws.com` and
`token.actions.githubusercontent.com:sub` equal to:

```text
repo:helmrdotdev/helmr:environment:release-production
```

Set the role maximum session duration to at least four hours so the workflow can poll long Image
Builder runs. The role permissions should cover only the worker-image OpenTofu stack and release
manifest verification: S3 state access, EC2 Image Builder pipeline/configuration resources, the
image-builder instance profile and role, required EC2 describe/distribution calls including
`ec2:DescribeImages`, public release AMI retention cleanup with `ec2:DeregisterImage` and
`ec2:DeleteSnapshot`, and `iam:PassRole` for the image-builder instance profile role.
