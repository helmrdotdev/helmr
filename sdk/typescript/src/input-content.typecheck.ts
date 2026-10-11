// Compile-only public API example: readonly conversation values remain valid
// Agent inputs, cron inputs and external Session enqueue values.
import { agent, triggers, type AgentDefinition, type ComputerDefinition, type Json } from "./agent"
import type { SessionRef } from "./contract"
import type { InputContent } from "./content"

export function inputContentUsage(computer: ComputerDefinition, session: SessionRef, input: InputContent): AgentDefinition<InputContent, Json, undefined> {
  void session.enqueue(input)
  return agent<InputContent, Json>({
    id: "conversation",
    computer,
    triggers: [triggers.cron("hourly", "0 * * * *", { timezone: "UTC", input })],
    turn: (turn) => turn.input,
  })
}
