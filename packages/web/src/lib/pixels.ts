export type Pixel = {
  x: number;
  y: number;
};

export const pixelsFromRows = (rows: readonly string[], filled = "#"): Pixel[] =>
  rows.flatMap((row, y) =>
    [...row].flatMap((cell, x) => (cell === filled ? [{ x, y }] : [])),
  );
