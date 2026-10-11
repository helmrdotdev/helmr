import { expect, test } from "bun:test";
import { composeExample, harnesses, interfaces, usecases } from "./messaging";

// Parse the same recipe the homepage renders, including setup and client usage.
// This does not establish provider integration or runtime behavior.
test("every selectable application sketch parses as TypeScript", () => {
  const transpiler = new Bun.Transpiler({ loader: "ts" });
  for (const usecase of usecases) {
    for (const harness of harnesses) {
      for (const transport of interfaces) {
        expect(() => transpiler.transformSync(composeExample(usecase, harness, transport))).not.toThrow();
      }
    }
  }
});
