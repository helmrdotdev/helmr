// The coding agents a starter can run on Helmr and the model providers each
// supports. This mirrors the starters in the CLI (`helmr init --template`); a
// browser test checks it against the setup instructions the server renders.

export type ProviderID = "anthropic" | "openai" | "opencode-go";
export type HarnessID = "claude-code" | "codex" | "opencode" | "pi";

export type Provider = {
  id: ProviderID;
  label: string;
  logo: string;
  secret: string;
  origin: string;
  keyURL: string;
};

export type Harness = {
  id: HarnessID;
  label: string;
  logo: string;
  // Default first.
  bindings: readonly { provider: ProviderID; model: string }[];
};

export const PROVIDERS: Record<ProviderID, Provider> = {
  anthropic: { id: "anthropic", label: "Anthropic", logo: "/logos/claude.svg", secret: "ANTHROPIC_API_KEY", origin: "https://api.anthropic.com", keyURL: "https://console.anthropic.com/settings/keys" },
  openai: { id: "openai", label: "OpenAI", logo: "/logos/openai.svg", secret: "OPENAI_API_KEY", origin: "https://api.openai.com", keyURL: "https://platform.openai.com/api-keys" },
  "opencode-go": { id: "opencode-go", label: "OpenCode Go", logo: "/logos/opencode.svg", secret: "OPENCODE_GO_API_KEY", origin: "https://opencode.ai", keyURL: "https://opencode.ai/auth" },
};

const MULTI = [
  { provider: "anthropic", model: "anthropic/claude-sonnet-5-5" },
  { provider: "openai", model: "openai/gpt-5.5" },
  { provider: "opencode-go", model: "opencode-go/glm-5.3" },
] as const;

export const HARNESSES: readonly Harness[] = [
  { id: "claude-code", label: "Claude Code", logo: "/logos/claude.svg", bindings: [{ provider: "anthropic", model: "claude-sonnet-5-5" }] },
  { id: "codex", label: "Codex", logo: "/logos/openai.svg", bindings: [{ provider: "openai", model: "gpt-5.5" }] },
  { id: "opencode", label: "OpenCode", logo: "/logos/opencode.svg", bindings: MULTI },
  { id: "pi", label: "Pi", logo: "/logos/pi.svg", bindings: MULTI },
];

// The prompt a person pastes into the coding agent they work with. It names
// where to read the instructions and which Project and Environment to use; it
// never carries a key.
export function setupPrompt(options: { origin: string; harness: HarnessID; provider: ProviderID; projectID: string; environmentID: string }): string {
  const url = `${options.origin}/agent-setup/${options.harness}.md?provider=${encodeURIComponent(options.provider)}`;
  return [
    "Set up a Helmr agent for me by following",
    url,
    "",
    `Server: ${options.origin}`,
    `Project: ${options.projectID}`,
    `Environment: ${options.environmentID}`,
  ].join("\n");
}
