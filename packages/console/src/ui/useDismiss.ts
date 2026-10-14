import { createEffect, onCleanup } from "solid-js";
import { openDismissLayer, type DismissReason } from "./dismiss";

// Closes a floating element on Escape or a press outside it while `open` is true.
// `contains` decides what counts as inside: normally the floating element and the
// control that opened it.
export function useDismiss(options: {
  open: () => boolean;
  contains: (target: Node) => boolean;
  onDismiss: (reason: DismissReason) => void;
}): void {
  createEffect(() => {
    if (!options.open()) return;
    const close = openDismissLayer({ contains: options.contains, dismiss: options.onDismiss });
    onCleanup(close);
  });
}
