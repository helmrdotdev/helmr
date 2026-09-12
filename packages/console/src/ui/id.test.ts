import { expect, test } from "bun:test";

import { formatID } from "./id";

test("shows the tail of long identifiers so shared prefixes stay distinguishable", () => {
  expect(formatID("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33")).toBe("…bb97bcc0dc33");
  expect(formatID("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc34")).toBe("…bb97bcc0dc34");
});

test("leaves short identifiers unchanged", () => {
  expect(formatID("review-pr")).toBe("review-pr");
  expect(formatID("0123456789abcdef")).toBe("0123456789abcdef");
  expect(formatID("")).toBe("");
});

test("keeps the digest algorithm and shows the digest tail", () => {
  const hex = "a".repeat(52) + "0123456789ab";
  expect(formatID(`sha256:${hex}`)).toBe("sha256:…0123456789ab");
  expect(formatID("sha256:abc")).toBe("sha256:abc");
});
