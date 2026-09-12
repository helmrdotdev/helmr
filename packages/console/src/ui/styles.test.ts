import { expect, test } from "bun:test";

import { actionMenuItemClass, ui } from "./styles";

const colourClasses = (value: string) => value.split(" ").filter((name) => /(^|:)text-console-/.test(name));

test("only the tone variant of a menu item sets its text colour", () => {
  expect(colourClasses(ui.actionMenuItemBase)).toEqual([]);
});

test("danger menu items use the danger text colour and never the default one", () => {
  const danger = actionMenuItemClass("danger");
  expect(danger).toContain(ui.actionMenuItemBase);
  expect(danger).toContain("text-console-danger-text");
  expect(colourClasses(danger)).not.toContain("text-console-text");
  expect(colourClasses(danger).every((name) => name.endsWith("text-console-danger-text"))).toBe(true);
});

test("default and undefined tones use the default text colour and never the danger one", () => {
  for (const tone of ["default", undefined] as const) {
    const value = actionMenuItemClass(tone);
    expect(value).toContain(ui.actionMenuItemBase);
    expect(value).toContain("text-console-text");
    expect(value).not.toContain("danger");
  }
});
