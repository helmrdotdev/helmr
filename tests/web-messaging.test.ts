import { expect, test } from "bun:test";
import { harnesses, interfaces, usecases } from "../packages/web/src/lib/messaging";

// These are editable application sketches, not provider integration tests. Parse
// every selectable composition so provider-local names cannot shadow Actor state.
test("selectable use-case, provider and interface snippets compose as TypeScript", () => {
  const transpiler = new Bun.Transpiler({ loader: "ts" });
  for (const usecase of usecases) {
    for (const harness of harnesses) {
      for (const transport of interfaces) {
        const code = `${usecase.head}
${harness.agent}
const approval = await tokens.create({ timeout: "30m" })
${transport.code}
const decision = await approval.wait({ schema: approvalSchema }).unwrap()
${usecase.action}
} })`;
        expect(() => transpiler.transformSync(code)).not.toThrow();
      }
    }
  }
});
