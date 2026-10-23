import { expect, test } from "bun:test"
import { analyze } from "../../../compiler/typescript/src/compile"
import * as production from "./production"

test("idle continuation residents compile automatic suspension while starter controls remain manual", () => {
  const { buildPlan } = analyze({
    architecture: "x86_64",
    exports: Object.entries(production).map(([exportName, value]) => ({
      exportName, value, modulePath: "helmr/app/entry-0.mjs",
    })),
  })
  expect(Object.fromEntries(buildPlan.definitions
    .filter(definition => definition.kind === "agent")
    .map(definition => [definition.declaredId, definition.manifest.suspension]))).toEqual({
    codex: "after_turn",
    claude: "after_turn",
    peer: "after_turn",
    models: "after_turn",
    receipt: "after_turn",
    deadline: "manual",
    "codex-starter": "manual",
    "claude-starter": "manual",
  })
})
