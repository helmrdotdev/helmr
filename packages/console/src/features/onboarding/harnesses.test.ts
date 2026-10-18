import { expect, test } from "bun:test";
import { HARNESSES, PROVIDERS, setupPrompt } from "./harnesses";

test("the setup prompt names the instructions, server, project and environment and nothing else", () => {
  expect(setupPrompt({ origin: "https://acme.helmr.dev", harness: "opencode", provider: "opencode-go", projectID: "acme", environmentID: "production" })).toBe(
    "Set up a Helmr agent for me by following\nhttps://acme.helmr.dev/agent-setup/opencode.md?provider=opencode-go\n\nServer: https://acme.helmr.dev\nProject: acme\nEnvironment: production",
  );
});

test("every harness offers known providers with Anthropic first where there is a choice", () => {
  for (const harness of HARNESSES) {
    expect(harness.bindings.length).toBeGreaterThan(0);
    for (const binding of harness.bindings) expect(PROVIDERS[binding.provider]).toBeDefined();
    if (harness.bindings.length > 1) expect(harness.bindings[0]!.provider).toBe("anthropic");
  }
  expect(HARNESSES.map((harness) => harness.label)).toEqual(["Claude Code", "Codex", "OpenCode", "Pi"]);
});
