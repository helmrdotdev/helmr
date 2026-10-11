import { computer, image } from "@helmr/sdk"

export const issueFixerComputer = computer({
  id: "issue-fixer",
  image: image("issue-fixer")
    .from("node:24-bookworm-slim")
    .run(["sh", "-ceu", "apt-get update && apt-get install -y --no-install-recommends git ripgrep ca-certificates && rm -rf /var/lib/apt/lists/*"])
    .env("ISSUE_FIXER_REPOSITORY", "/workspace/repository")
    .workdir("/workspace/repository"),
  resources: { cpu: 2, memory: "4GiB" },
  // Add preparation for your repository and stable Secret ID bindings before use.
})
