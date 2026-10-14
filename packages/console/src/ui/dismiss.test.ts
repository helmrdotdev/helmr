import { afterEach, expect, test } from "bun:test";

import { dismissOnKey, dismissOnPointer, openDismissLayer, type DismissReason } from "./dismiss";

const closers: Array<() => void> = [];
afterEach(() => {
  while (closers.length) closers.pop()!();
});

function layer(inside: Node[] = []) {
  const reasons: DismissReason[] = [];
  closers.push(openDismissLayer({ contains: (target) => inside.includes(target), dismiss: (reason) => reasons.push(reason) }));
  return reasons;
}

function key(name: string, isComposing = false) {
  const event = { key: name, isComposing, prevented: false, stopped: false, preventDefault() { this.prevented = true; }, stopPropagation() { this.stopped = true; } };
  return event;
}

const node = (name: string) => ({ name }) as unknown as Node;

test("Escape closes only the topmost layer and stops the event", () => {
  const outer = layer();
  const inner = layer();
  const event = key("Escape");
  expect(dismissOnKey(event)).toBe(true);
  expect(inner).toEqual(["escape"]);
  expect(outer).toEqual([]);
  expect(event.prevented && event.stopped).toBe(true);
});

test("other keys and an empty stack leave the event alone", () => {
  const reasons = layer();
  const other = key("Enter");
  expect(dismissOnKey(other)).toBe(false);
  expect(reasons).toEqual([]);
  closers.pop()!();
  const escape = key("Escape");
  expect(dismissOnKey(escape)).toBe(false);
  expect(escape.stopped).toBe(false);
});

test("a press inside a layer keeps it and the layers below open", () => {
  const panel = node("outer panel");
  const outer = layer([panel]);
  const inner = layer();
  dismissOnPointer(panel);
  expect(inner).toEqual(["outside"]);
  expect(outer).toEqual([]);
});

test("a press outside every layer closes them all", () => {
  const outer = layer([node("a")]);
  const inner = layer([node("b")]);
  dismissOnPointer(node("page"));
  expect(outer).toEqual(["outside"]);
  expect(inner).toEqual(["outside"]);
});

test("Escape during text composition is left to the input method", () => {
  const reasons = layer();
  const event = key("Escape", true);
  expect(dismissOnKey(event)).toBe(false);
  expect(event.stopped).toBe(false);
  expect(reasons).toEqual([]);
});

test("a closed layer no longer receives Escape, and closing twice is harmless", () => {
  const reasons: DismissReason[] = [];
  const close = openDismissLayer({ contains: () => false, dismiss: (reason) => reasons.push(reason) });
  close();
  close();
  expect(dismissOnKey(key("Escape"))).toBe(false);
  expect(reasons).toEqual([]);
});
