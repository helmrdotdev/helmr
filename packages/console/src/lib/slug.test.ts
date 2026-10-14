import { expect, test } from "bun:test";

import { slugify } from "./slug";

test("lowercases and joins words with single hyphens", () => {
  expect(slugify("  My  Sales   App ")).toBe("my-sales-app");
});

test("drops characters outside letters, digits and hyphens", () => {
  expect(slugify("Prod (EU) #2!")).toBe("prod-eu-2");
  expect(slugify("Café")).toBe("caf");
});

test("collapses hyphen runs and trims edge hyphens", () => {
  expect(slugify("--a---b--")).toBe("a-b");
});

test("caps the length at 48 without leaving a trailing hyphen", () => {
  const value = slugify(`${"a".repeat(47)} b`);
  expect(value).toBe("a".repeat(47));
  expect(slugify("x".repeat(60))).toHaveLength(48);
});

test("empty and symbol-only names give an empty slug", () => {
  expect(slugify("")).toBe("");
  expect(slugify("!!!")).toBe("");
});
