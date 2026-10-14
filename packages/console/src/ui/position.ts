export type Rect = { top: number; bottom: number; left: number; right: number; width: number };

export type Placement = {
  top: number;
  maxHeight: number;
  left?: number;
  right?: number;
};

const GAP = 6;
const EDGE = 8;
const MIN_HEIGHT = 80;

// Places a floating panel under its anchor, or above it when there is clearly more
// room there, aligned to the anchor's start or end edge and kept 8px inside the
// viewport. `height` is the panel's natural height, capped by `maxHeight`.
export function placePopover(options: {
  anchor: Rect;
  width: number;
  height: number;
  viewport: { width: number; height: number };
  align: "start" | "end";
  maxHeight: number;
}): Placement {
  const { anchor, viewport } = options;
  const wanted = Math.min(options.height, options.maxHeight);
  const below = viewport.height - anchor.bottom - GAP - EDGE;
  const above = anchor.top - GAP - EDGE;
  const placeAbove = below < Math.min(wanted, 120) && above > below;
  const maxHeight = Math.max(MIN_HEIGHT, Math.min(options.maxHeight, placeAbove ? above : below));
  const height = Math.min(wanted, maxHeight);
  const top = placeAbove ? Math.max(EDGE, anchor.top - GAP - height) : anchor.bottom + GAP;
  if (options.align === "end") {
    const right = viewport.width - anchor.right;
    return { top, maxHeight, right: Math.max(EDGE, Math.min(right, viewport.width - EDGE - options.width)) };
  }
  return { top, maxHeight, left: Math.max(EDGE, Math.min(anchor.left, viewport.width - EDGE - options.width)) };
}
