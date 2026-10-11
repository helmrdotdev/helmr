import { test, expect } from "bun:test"
import { CONTENT_BYTES, ContentError, contentBytes, normalizeContent, normalizeInput } from "./content"

test("Content normalization preserves empty and atomic boundaries with canonical bytes", () => {
  expect(normalizeContent("")).toEqual([{ type: "text", text: "" }])
  expect(normalizeContent([], false)).toEqual([])
  const input = [{ type: "text", text: "a\u0000日" }, { type: "text", text: "b" }, { type: "json", value: { z: 1.0, a: false } }]
  const expected = '[{"text":"a\\u0000日","type":"text"},{"text":"b","type":"text"},{"type":"json","value":{"a":false,"z":1}}]'
  const normalized = normalizeContent(input, false)
  expect(JSON.stringify(normalized)).toBe(expected)
  expect(contentBytes(normalized)).toBe(new TextEncoder().encode(expected).length)
  input[0]!.text = "changed"
  expect(normalized[0]).toEqual({ type: "text", text: "a\u0000日" })
})

test("Content applies the same inclusive byte and part bounds", () => {
  const overhead = contentBytes(normalizeContent(""))
  expect(contentBytes(normalizeContent("x".repeat(CONTENT_BYTES - overhead)))).toBe(CONTENT_BYTES)
  expect(() => normalizeContent("x".repeat(CONTENT_BYTES - overhead + 1))).toThrow(ContentError)
  expect(normalizeContent(Array.from({ length: 64 }, () => ({ type: "json", value: null })))).toHaveLength(64)
  expect(() => normalizeContent(Array.from({ length: 65 }, () => ({ type: "json", value: null })))).toThrow("64 parts")
  for (const invalid of [null, {}, [{ type: "text", text: null }], [{ type: "text", text: "\ud800" }], [{ type: "json", value: Infinity }]]) expect(() => normalizeContent(invalid)).toThrow(ContentError)
  expect(() => normalizeContent([{ type: "file", file: "anything" }])).toThrow("unsupported")
})

test("Input requires a text-part array and preserves text exactly", () => {
  expect(normalizeInput([{ type: "text", text: "Hello" }, { type: "text", text: " world\n" }])).toEqual([{ type: "text", text: "Hello" }, { type: "text", text: " world\n" }])
  for (const invalid of [null, "hello", { type: "message", content: [] }, { task: "job" }, [{ type: "json", value: null }], [{ type: "file", id: "file" }]]) expect(() => normalizeInput(invalid)).toThrow(ContentError)
})
