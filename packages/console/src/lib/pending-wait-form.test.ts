import { expect, test } from "bun:test";

import {
  parsePendingWaitCompletion,
  pendingTokenID,
  pendingWaitCompletionErrorMessage,
  pendingWaitForm,
} from "./pending-wait-form";

test("selects only declared version 1 form types", () => {
  expect(pendingWaitForm({ form: { version: 1, type: "approval" } })).toEqual({ kind: "approval" });
  expect(pendingWaitForm({ form: { version: 1, type: "message" } })).toEqual({ kind: "message" });
  expect(pendingWaitForm({ form: { version: 1, type: "json" } })).toEqual({ kind: "json" });
});

test("falls back to JSON for missing, unknown, or malformed form metadata", () => {
  expect(pendingWaitForm(undefined)).toEqual({ kind: "json" });
  expect(pendingWaitForm({ form: { version: 1, type: "future" } })).toEqual({ kind: "json" });
  expect(pendingWaitForm({ form: { version: 2, type: "approval" } })).toEqual({ kind: "json" });
  expect(pendingWaitForm({ form: "approval" })).toEqual({ kind: "json" });
});

test("parses approval, unchanged message, and generic JSON payloads", () => {
  expect(parsePendingWaitCompletion("approval", "", true)).toEqual({ approved: true });
  expect(parsePendingWaitCompletion("approval", "", false)).toEqual({ approved: false });
  expect(parsePendingWaitCompletion("message", "  keep spacing  ")).toEqual({ text: "  keep spacing  " });
  expect(parsePendingWaitCompletion("json", "[1, {\"ok\": true}]")).toEqual([1, { ok: true }]);
});

test("rejects whitespace-only messages and invalid JSON", () => {
  expect(() => parsePendingWaitCompletion("message", " \n\t ")).toThrow("Message is required.");
  expect(() => parsePendingWaitCompletion("json", "{")).toThrow("Enter valid JSON.");
});

test("reads the typed token id without treating a wait id as a token", () => {
  expect(pendingTokenID({ token_id: "tok_123" })).toBe("tok_123");
  expect(pendingTokenID({ token_id: " " })).toBeNull();
  expect(pendingTokenID({ wait_id: "wait_123" })).toBeNull();
});

test("surfaces completion conflicts without suggesting an overwrite", () => {
  expect(pendingWaitCompletionErrorMessage({ status: 409 })).toBe(
    "This wait was already completed with different data. Nothing was changed.",
  );
  expect(pendingWaitCompletionErrorMessage(new Error("Network unavailable."))).toBe("Network unavailable.");
});
