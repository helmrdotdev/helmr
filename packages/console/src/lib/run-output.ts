import type { Run, TaskOutput } from "./runs";

export type TaskOutputKind = "null" | "boolean" | "number" | "string" | "array" | "object" | "unknown";
export type TaskOutputRenderMode = "primitive" | "text" | "json" | "table";

export type TaskOutputTable = {
  columns: string[];
  rows: string[][];
};

export function hasRunOutput(run: Pick<Run, "output">): boolean {
  return run.output !== undefined;
}

export function taskOutputKind(output: TaskOutput): TaskOutputKind {
  if (output === null) return "null";
  if (Array.isArray(output)) return "array";
  switch (typeof output) {
    case "boolean":
      return "boolean";
    case "number":
      return "number";
    case "string":
      return "string";
    case "object":
      return "object";
    default:
      return "unknown";
  }
}

export function formatTaskOutput(output: TaskOutput): string {
  if (typeof output === "string") return output;
  const formatted = JSON.stringify(output, null, 2);
  return formatted === undefined ? String(output) : formatted;
}

export function taskOutputRenderMode(output: TaskOutput): TaskOutputRenderMode {
  if (taskOutputTable(output) !== null) return "table";
  if (typeof output === "string") return "text";
  if (output === null || typeof output === "boolean" || typeof output === "number") return "primitive";
  return "json";
}

export function taskOutputTable(output: TaskOutput): TaskOutputTable | null {
  if (Array.isArray(output)) return arrayTable(output);
  const record = objectRecord(output);
  if (record === null || Object.keys(record).length === 0) return null;
  return {
    columns: ["key", "value"],
    rows: Object.entries(record).map(([key, value]) => [key, formatTableCell(value)]),
  };
}

function arrayTable(value: unknown[]): TaskOutputTable | null {
  if (value.length === 0) return null;
  if (value.every((item) => objectRecord(item) !== null)) {
    const records = value.map((item) => objectRecord(item) ?? {});
    const columns = uniqueColumns(records.flatMap((record) => Object.keys(record)));
    if (columns.length === 0) return null;
    return {
      columns,
      rows: records.map((record) => columns.map((column) => formatTableCell(record[column]))),
    };
  }
  if (value.every(Array.isArray)) {
    const rows = value as unknown[][];
    const width = Math.max(...rows.map((row) => row.length));
    if (width === 0) return null;
    const columns = Array.from({ length: width }, (_, index) => String(index));
    return {
      columns,
      rows: rows.map((row) => columns.map((_, index) => formatTableCell(row[index]))),
    };
  }
  return null;
}

function objectRecord(value: unknown): Record<string, unknown> | null {
  if (value === null || typeof value !== "object" || Array.isArray(value)) return null;
  return value as Record<string, unknown>;
}

function uniqueColumns(columns: string[]): string[] {
  const seen = new Set<string>();
  const result: string[] = [];
  for (const column of columns) {
    if (seen.has(column)) continue;
    seen.add(column);
    result.push(column);
  }
  return result;
}

function formatTableCell(value: unknown): string {
  if (value === undefined) return "";
  if (typeof value === "string") return value;
  const formatted = JSON.stringify(value);
  return formatted === undefined ? String(value) : formatted;
}
