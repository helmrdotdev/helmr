import { expect, test } from "bun:test";

import { createRowActions } from "./rowActions";

test("marks only the running row and action as busy", async () => {
  const rows = createRowActions<"remove" | "role">();
  let release!: () => void;
  const running = rows.run("a", "remove", () => new Promise<void>((resolve) => (release = resolve)), String);
  expect(rows.busy("a")).toBe(true);
  expect(rows.busy("a", "remove")).toBe(true);
  expect(rows.busy("a", "role")).toBe(false);
  expect(rows.busy("b")).toBe(false);
  release();
  expect(await running).toBe(true);
  expect(rows.busy("a")).toBe(false);
});

test("keeps a failure on its row until the next action", async () => {
  const rows = createRowActions();
  expect(await rows.run("a", "default", () => Promise.reject(new Error("nope")), (error) => (error as Error).message)).toBe(false);
  expect(rows.error("a")).toBe("nope");
  expect(rows.error("b")).toBeNull();
  await rows.run("b", "default", async () => {}, String);
  expect(rows.error("a")).toBeNull();
});

test("refuses a second action while one is running", async () => {
  const rows = createRowActions();
  let release!: () => void;
  const first = rows.run("a", "default", () => new Promise<void>((resolve) => (release = resolve)), String);
  let ran = false;
  expect(await rows.run("b", "default", async () => { ran = true; }, String)).toBe(false);
  expect(ran).toBe(false);
  expect(rows.busy("a")).toBe(true);
  release();
  await first;
  expect(rows.busy("a")).toBe(false);
});
