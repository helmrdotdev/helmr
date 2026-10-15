import type { JsonValue } from "../../../../sdk/typescript/src/contract";

// Limits that keep large payloads cheap to render: a tree shows children in
// pages, clamps long strings, and is skipped entirely for very large values.
export const childPage = 100;
export const stringClamp = 1024;
export const treeLimit = 64 * 1024;
export const textLineClamp = 6;

const identifier = /^[A-Za-z_$][A-Za-z0-9_$]*$/;

// A one-line summary: first keys with short values, nested values shrunk to
// {…} and [n]. Used for collapsed nodes and log lines.
export function preview(value: JsonValue, budget = 80): string {
  const out = summary(value, 0);
  return out.length > budget ? `${out.slice(0, budget - 1)}…` : out;
}

function summary(value: JsonValue, depth: number): string {
  if (value === null || typeof value !== "object") return typeof value === "string" ? JSON.stringify(value.length > 24 ? `${value.slice(0, 24)}…` : value) : String(value);
  if (Array.isArray(value)) return depth > 0 ? `[${value.length}]` : `[${value.slice(0, 4).map(item => summary(item, 1)).join(", ")}${value.length > 4 ? ", …" : ""}]`;
  if (depth > 0) return "{…}";
  const entries = Object.entries(value);
  const parts = entries.slice(0, 6).map(([key, item]) => `${identifier.test(key) ? key : JSON.stringify(key)}: ${summary(item, 1)}`);
  return `{${parts.join(", ")}${entries.length > 6 ? ", …" : ""}}`;
}

// A scalar as JSON. A string keeps its escaped "\n" and also breaks the line
// there, so multi-line text reads as lines while staying valid JSON.
export function scalar(value: JsonValue, clamp = Infinity): string {
  if (typeof value !== "string") return String(value);
  const text = value.length > clamp ? `${value.slice(0, clamp)}…` : value;
  return `"${text.split("\n").map(line => JSON.stringify(line).slice(1, -1)).join("\\n\n")}"`;
}

// Whether a node path matches an open pattern; "*" matches any one key, so
// "data.*" opens every content part.
export function pathMatches(patterns: ReadonlySet<string> | undefined, path: readonly (string | number)[]): boolean {
  if (!patterns) return false;
  for (const pattern of patterns) {
    const parts = pattern.split(".");
    if (parts.length === path.length && parts.every((part, index) => part === "*" || part === String(path[index]))) return true;
  }
  return false;
}

// The path of a node for copying, e.g. $.items[0]["content-type"].
export function jsonPath(path: readonly (string | number)[]): string {
  return path.reduce<string>((out, part) =>
    typeof part === "number" ? `${out}[${part}]` : identifier.test(part) ? `${out}.${part}` : `${out}[${JSON.stringify(part)}]`, "$");
}

export function byteLength(text: string): number {
  return new TextEncoder().encode(text).length;
}

export function formatBytes(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${(bytes / 1024 / 1024).toFixed(1)} MB`;
}
