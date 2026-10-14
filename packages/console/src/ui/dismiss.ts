// Dismissal for everything that floats over the page (popovers, menus, pickers).
// Open layers form a stack: Escape closes only the topmost layer, and a pointer
// press closes every layer above the one it lands in. Document listeners are
// installed while at least one layer is open and run in the capture phase, so an
// Escape that closes a popover never also closes the dialog underneath it.

export type DismissReason = "outside" | "escape" | "tab" | "blur";

export type DismissLayer = {
  contains: (target: Node) => boolean;
  dismiss: (reason: DismissReason) => void;
};

type KeyEventLike = { key: string; isComposing?: boolean; preventDefault(): void; stopPropagation(): void };

const layers: DismissLayer[] = [];

export function openDismissLayer(layer: DismissLayer): () => void {
  layers.push(layer);
  if (layers.length === 1) listen(true);
  return () => {
    const index = layers.indexOf(layer);
    if (index < 0) return;
    layers.splice(index, 1);
    if (layers.length === 0) listen(false);
  };
}

// Escape while an input method is composing text belongs to the input method.
export function dismissOnKey(event: KeyEventLike): boolean {
  if (event.key !== "Escape" || event.isComposing) return false;
  const top = layers[layers.length - 1];
  if (!top) return false;
  event.preventDefault();
  event.stopPropagation();
  top.dismiss("escape");
  return true;
}

export function dismissOnPointer(target: Node | null): void {
  for (const layer of [...layers].reverse()) {
    if (target && layer.contains(target)) return;
    layer.dismiss("outside");
  }
}

const onKeyDown = (event: KeyboardEvent) => {
  dismissOnKey(event);
};

const onPointerDown = (event: PointerEvent) => {
  dismissOnPointer(event.target instanceof Node ? event.target : null);
};

function listen(on: boolean): void {
  if (typeof document === "undefined") return;
  if (on) {
    document.addEventListener("keydown", onKeyDown, true);
    document.addEventListener("pointerdown", onPointerDown, true);
  } else {
    document.removeEventListener("keydown", onKeyDown, true);
    document.removeEventListener("pointerdown", onPointerDown, true);
  }
}
