import { expect, test } from "bun:test";
import { byteLength, formatBytes, jsonPath, preview } from "./payload";

test("previews the first keys and shrinks nested values", () => {
  expect(preview({ shard: 3, passed: 41, failed: [{ test: "a" }, { test: "b" }], meta: { a: 1 } })).toBe("{shard: 3, passed: 41, failed: [2], meta: {…}}");
  expect(preview([1, "two", null, true, 5])).toBe('[1, "two", null, true, …]');
  expect(preview({ "content-type": "text/plain" })).toBe('{"content-type": "text/plain"}');
  expect(preview({ text: "x".repeat(200) }, 20)).toBe('{text: "xxxxxxxxxxx…');
});

test("builds copyable paths", () => {
  expect(jsonPath([])).toBe("$");
  expect(jsonPath(["items", 0, "content-type"])).toBe('$.items[0]["content-type"]');
});

test("sizes payloads in bytes", () => {
  expect(byteLength("雪")).toBe(3);
  expect(formatBytes(412)).toBe("412 B");
  expect(formatBytes(6963)).toBe("6.8 KB");
  expect(formatBytes(1887436)).toBe("1.8 MB");
});

test("multi-line strings keep escapes and break at them", async () => {
  const { scalar } = await import("./payload");
  expect(scalar("a\nb")).toBe('"a\\n\nb"');
  expect(scalar('say "hi"')).toBe('"say \\"hi\\""');
  expect(scalar(3)).toBe("3");
});

test("open patterns match exact paths and wildcard keys", async () => {
  const { pathMatches, preview } = await import("./payload");
  const patterns = new Set(["data", "data.*.value"]);
  expect(pathMatches(patterns, ["data"])).toBe(true);
  expect(pathMatches(patterns, ["data", 3, "value"])).toBe(true);
  expect(pathMatches(patterns, ["data", 3])).toBe(false);
  expect(preview({ text: "a\nb" })).toBe('{text: "a\\nb"}');
});
