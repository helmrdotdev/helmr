import type { OpenCode } from "@opencode/sdk/effect"

export const model = "opencode-go/glm-5.3"
export const introduction = `Help the user build a small website in the current workspace.
Ask about missing requirements before making a substantial change. Write ordinary
HTML, CSS and JavaScript files that the user can inspect and edit. Explain which
files changed, how to view them, and what you checked. Continue improving those
files when the user follows up. Do not publish the site or claim that it is hosted.`

// This application keeps command/edit decisions with the human. These rules
// govern native tools, not the filesystem access of an approved shell command.
export function nativeOptions(directory: string): OpenCode.CreateOptions {
  return {
    config: {
      directory, project: false, claude: false,
      content: JSON.stringify({
        update: "disable", share: "disabled",
        permissions: [
          { action: "*", resource: "*", effect: "ask" },
          { action: "read", resource: "*", effect: "allow" },
          { action: "read", resource: "*.env", effect: "ask" },
          { action: "read", resource: "*.env.*", effect: "ask" },
          { action: "read", resource: "*.env.example", effect: "allow" },
          ...["glob", "grep", "question"].map(action => ({ action, resource: "*", effect: "allow" })),
          { action: "edit", resource: ".helmr/**", effect: "deny" },
          { action: "edit", resource: "**/.helmr/**", effect: "deny" },
          { action: "subagent", resource: "*", effect: "deny" },
          { action: "external_directory", resource: "*", effect: "deny" },
        ],
        policies: [
          { action: "provider.use", resource: "*", effect: "deny" },
          { action: "provider.use", resource: "opencode-go", effect: "allow" },
        ],
      }),
    },
  }
}
