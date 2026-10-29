import "./browser-test-env";
import { expect, test } from "bun:test";
import { isPaletteShortcut } from "./shortcuts";

const key = (apple: boolean, overrides: Partial<Parameters<typeof isPaletteShortcut>[0]>) =>
  isPaletteShortcut({ key: "k", metaKey: false, ctrlKey: false, altKey: false, shiftKey: false, isComposing: false, keyCode: 75, ...overrides }, apple);

test("on Apple platforms only Cmd+K opens the palette", () => {
  expect(key(true, { metaKey: true })).toBe(true);
  expect(key(true, { metaKey: true, key: "K" })).toBe(true);
  expect(key(true, { ctrlKey: true })).toBe(false);
  expect(key(true, { metaKey: true, ctrlKey: true })).toBe(false);
  expect(key(true, {})).toBe(false);
  expect(key(true, { metaKey: true, shiftKey: true })).toBe(false);
  expect(key(true, { metaKey: true, key: "j" })).toBe(false);
});

test("elsewhere only Ctrl+K opens the palette", () => {
  expect(key(false, { ctrlKey: true })).toBe(true);
  expect(key(false, { metaKey: true })).toBe(false);
  expect(key(false, { ctrlKey: true, altKey: true })).toBe(false);
  expect(key(false, {})).toBe(false);
});

test("the shortcut never fires while an input method composes", () => {
  expect(key(true, { metaKey: true, isComposing: true })).toBe(false);
  expect(key(false, { ctrlKey: true, keyCode: 229 })).toBe(false);
});
