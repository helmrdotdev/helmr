import { expect, test } from "bun:test"
import { readFileSync } from "node:fs"
import { parseSession, parseTurnState } from "./session"
const fixtures = JSON.parse(readFileSync(new URL("../../../../tests/fixtures/session-wire.json", import.meta.url), "utf8"))
for (const [name, wire] of Object.entries(fixtures)) {
  test(`CLI and client wire fixture: ${name}`, () => {
    const parsed = name === "session" ? parseSession(wire) : parseTurnState(wire)
    expect(parsed.id).toBe((wire as { id: string }).id)
    if (name === "completed-expired") expect(Object.hasOwn(parsed, "result")).toBe(false)
    if (name === "completed-null") expect(parsed).toHaveProperty("result", null)
  })
}
