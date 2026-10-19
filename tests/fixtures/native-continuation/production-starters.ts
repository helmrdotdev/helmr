// Keep optional embedded SDK module effects out of ordinary continuation tests.
export * from "./production"
import { workspace } from "./production"

import { createEmbeddedStarter } from "./embedded-starters"
export const opencodeStarter = createEmbeddedStarter("opencode", workspace)
export const piStarter = createEmbeddedStarter("pi", workspace)

import { createProtectedStarter } from "./protected-starters"
export const protectedClaude = createProtectedStarter("claude", workspace)
export const protectedCodex = createProtectedStarter("codex", workspace)
export const protectedOpenCodeAnthropic = createProtectedStarter("opencode-anthropic", workspace)
export const protectedOpenCodeOpenAI = createProtectedStarter("opencode-openai", workspace)
export const protectedOpenCodeGo = createProtectedStarter("opencode-go", workspace)
export const protectedPiAnthropic = createProtectedStarter("pi-anthropic", workspace)
export const protectedPiOpenAI = createProtectedStarter("pi-openai", workspace)
export const protectedPiGo = createProtectedStarter("pi-go", workspace)
