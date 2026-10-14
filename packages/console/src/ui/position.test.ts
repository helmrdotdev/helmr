import { expect, test } from "bun:test";

import { placePopover } from "./position";

const viewport = { width: 1000, height: 800 };
const anchor = (top: number, left: number, width = 100, height = 28) => ({ top, bottom: top + height, left, right: left + width, width });

test("opens below the anchor, start-aligned", () => {
  expect(placePopover({ anchor: anchor(100, 200), width: 160, height: 120, viewport, align: "start", maxHeight: 260 }))
    .toEqual({ top: 134, maxHeight: 260, left: 200 });
});

test("end alignment keeps the right edges together", () => {
  expect(placePopover({ anchor: anchor(100, 800), width: 160, height: 120, viewport, align: "end", maxHeight: 260 }))
    .toEqual({ top: 134, maxHeight: 260, right: 100 });
});

test("flips above when the space below is too small", () => {
  const placement = placePopover({ anchor: anchor(740, 200), width: 160, height: 200, viewport, align: "start", maxHeight: 260 });
  expect(placement.top).toBe(740 - 6 - 200);
  expect(placement.maxHeight).toBe(260);
});

test("stays 8px inside the viewport horizontally", () => {
  expect(placePopover({ anchor: anchor(100, 950, 40), width: 200, height: 100, viewport, align: "start", maxHeight: 260 }).left).toBe(792);
  expect(placePopover({ anchor: anchor(100, 0, 40), width: 200, height: 100, viewport, align: "end", maxHeight: 260 }).right).toBe(792);
  expect(placePopover({ anchor: anchor(100, 2, 40), width: 200, height: 100, viewport, align: "start", maxHeight: 260 }).left).toBe(8);
});

test("caps the height by the room on the chosen side", () => {
  const placement = placePopover({ anchor: anchor(100, 200), width: 160, height: 200, viewport: { width: 1000, height: 200 }, align: "start", maxHeight: 260 });
  expect(placement).toEqual({ top: 8, maxHeight: 86, left: 200 });
});
