import { expect, test } from "bun:test"
import { analyze } from "../../../compiler/typescript/src/compile"
import * as production from "./production"

test("idle continuation residents compile automatic hibernation while starter controls stay manual", () => {
  const { buildPlan } = analyze({
    architecture: "x86_64",
    exports: Object.entries(production).map(([exportName, value]) => ({
      exportName, value, modulePath: "helmr/app/entry-0.mjs",
    })),
  })
  expect(Object.fromEntries(buildPlan.definitions
    .filter(definition => definition.kind === "agent")
    .map(definition => [definition.declaredId, definition.manifest.hibernateAfterIdleMs]))).toEqual({
    codex: 1000,
    claude: 1000,
    peer: 1000,
    models: 1000,
    receipt: 1000,
    deadline: null,
    "codex-starter": null,
    "claude-starter": null,
  })
})
