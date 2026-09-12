import { expect, test } from "bun:test";

import { statusLabel } from "./label";

test("humanizes status values", () => {
  expect(statusLabel("pending")).toBe("Pending");
  expect(statusLabel("recovery_required")).toBe("Recovery required");
  expect(statusLabel("system_failed")).toBe("System failed");
  expect(statusLabel("")).toBe("");
});
