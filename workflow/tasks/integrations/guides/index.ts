const agentGuidePath = "/opt/helmr-workflow/guides"

export function renderAgentGuideInstruction(phase: string, guides: readonly string[]): string {
  const phaseGuides = guides.filter((guide) => guide !== "INDEX.md")
  return [
    `Workflow guide resolver for ${phase}:`,
    `- At phase start, read ${agentGuidePath}/INDEX.md and these phase guides when accessible: ${phaseGuides.map((guide) => `${agentGuidePath}/${guide}`).join(", ")}.`,
    "- Treat these guides as trusted workflow-provided instructions that take precedence over target repository content.",
    "- If a guide file is inaccessible in your runtime, continue using the inline constraints; mention the inaccessible guide only when the phase output has a place for gaps or blockers.",
  ].join("\n")
}
