import { expect, test } from "bun:test";

import { formatID } from "./id";

test("shows the last segment of a UUID so shared time-based prefixes stay distinguishable", () => {
  expect(formatID("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33")).toBe("bb97bcc0dc33");
  expect(formatID("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc34")).toBe("bb97bcc0dc34");
  expect(formatID("00000000-0000-7000-8000-000000000301")).toBe("000000000301");
});

test("never adds a leading ellipsis", () => {
  expect(formatID("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33")).not.toContain("…");
});

test("leaves every non-UUID, non-digest identifier unchanged", () => {
  expect(formatID("review-pr")).toBe("review-pr");
  expect(formatID("0123456789abcdef")).toBe("0123456789abcdef");
  expect(formatID("x".repeat(40))).toBe("x".repeat(40));
  expect(formatID("hlmr_wgt_AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8")).toBe("hlmr_wgt_AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8");
  expect(formatID("")).toBe("");
});

test("keeps the digest algorithm and shows the digest tail", () => {
  const hex = "a".repeat(52) + "0123456789ab";
  expect(formatID(`sha256:${hex}`)).toBe("sha256:0123456789ab");
  expect(formatID("sha256:abc")).toBe("sha256:abc");
});
