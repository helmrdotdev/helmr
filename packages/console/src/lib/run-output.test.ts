import { expect, test } from "bun:test";

import { formatTaskOutput, hasRunOutput, taskOutputKind, taskOutputRenderMode, taskOutputTable } from "./run-output";

test("detects explicit run output, including null", () => {
  expect(hasRunOutput({ output: { ok: true } })).toBe(true);
  expect(hasRunOutput({ output: null })).toBe(true);
  expect(hasRunOutput({})).toBe(false);
});

test("classifies task output by primitive runtime type", () => {
  expect(taskOutputKind(null)).toBe("null");
  expect(taskOutputKind(true)).toBe("boolean");
  expect(taskOutputKind(42)).toBe("number");
  expect(taskOutputKind("done")).toBe("string");
  expect(taskOutputKind(["a"])).toBe("array");
  expect(taskOutputKind({ report: "weekly report" })).toBe("object");
});

test("formats string output as plain text", () => {
  expect(formatTaskOutput("done")).toBe("done");
});

test("formats non-string output as pretty JSON", () => {
  expect(formatTaskOutput({ report: "weekly report" })).toBe('{\n  "report": "weekly report"\n}');
  expect(formatTaskOutput(["a", 1])).toBe('[\n  "a",\n  1\n]');
  expect(formatTaskOutput(null)).toBe("null");
});

test("renders array records as a structural table", () => {
  expect(taskOutputRenderMode([{ id: 1, status: "ok" }, { id: 2, extra: true }])).toBe("table");
  expect(taskOutputTable([{ id: 1, status: "ok" }, { id: 2, extra: true }])).toEqual({
    columns: ["id", "status", "extra"],
    rows: [["1", "ok", ""], ["2", "", "true"]],
  });
});

test("renders array tuples as a structural table", () => {
  expect(taskOutputTable([["name", "count"], ["runs", 12]])).toEqual({
    columns: ["0", "1"],
    rows: [["name", "count"], ["runs", "12"]],
  });
});

test("renders flat objects as key value tables", () => {
  expect(taskOutputRenderMode({ ok: true, value: null })).toBe("table");
  expect(taskOutputTable({ ok: true, value: null })).toEqual({
    columns: ["key", "value"],
    rows: [["ok", "true"], ["value", "null"]],
  });
});

test("keeps mixed arrays in JSON mode", () => {
  expect(taskOutputRenderMode(["a", { id: 1 }])).toBe("json");
  expect(taskOutputTable(["a", { id: 1 }])).toBeNull();
});
