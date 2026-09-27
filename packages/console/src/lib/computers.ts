import { postJson, request } from "./api";

export type ComputerSecret =
  | { secret: string; env: { name: string; mode: "raw"; allowed_origins?: never } | { name: string; mode: "protected"; allowed_origins: string[] }; file?: never }
  | { secret: string; file: { path: string }; env?: never };

export type ComputerStatus = "available" | "deleted" | "deleting";

export type Computer = {
  id: string;
  key?: string;
  sandbox_id: string;
  deployment_id: string;
  status: ComputerStatus;
  residency: "cold" | "starting" | "running" | "parking" | "parked" | "restoring" | "unavailable";
  error?: { code: string; message: string };
  secrets: ComputerSecret[];
  last_activity_at: string;
  created_at: string;
  updated_at: string;
};

export type ComputerListItem = Omit<Computer, "secrets">;

export type ListComputersResponse = {
  computers: ComputerListItem[];
  next_cursor?: string;
};

export type ComputerScope = {
  projectID: string;
  environmentID: string;
};

export type CreateComputerInput = {
  key?: string;
  secrets?: ComputerSecret[];
  idempotency_key: string;
};

export type CommandStatus = "pending" | "starting" | "running" | "stopping" | "exited" | "failed" | "cancelled" | "timed_out" | "lost";

export type CommandReceipt = { command_id: string };
export type CommandInfo = {
  id: string;
  computer_id: string;
  status: CommandStatus;
  process_reconciled: boolean;
  outcome?: {
    command_id: string;
    kind: "exited" | "cancelled" | "timed_out" | "system_failed";
    terminal_at: string;
    exit_code?: number;
    failure?: { reason: "guest_failure" | "placement_failed" | "scope_termination_failed" };
  };
};

export type CommandLogRecord = {
  kind: "output";
  stream: "stdout" | "stderr";
  cursor: string;
  content_base64: string;
  observed_at: string;
} | {
  kind: "gap";
  stream: "stdout" | "stderr";
  cursor: string;
  from_sequence: string;
  through_sequence: string;
};

export type CommandLogPage = {
  logs: CommandLogRecord[];
  output_state: "open" | "closed" | "unavailable";
  next_cursor?: string;
};

export type ExecComputerInput = {
  command: string[];
  cwd?: string;
  timeout?: string;
  idempotency_key: string;
};

export async function listComputers(
  scope: ComputerScope,
  options: { cursor?: string | undefined; limit?: number | undefined } = {},
): Promise<ListComputersResponse> {
  const params = new URLSearchParams();
  if (options.cursor) params.set("cursor", options.cursor);
  if (options.limit !== undefined) params.set("limit", String(options.limit));
  const query = params.size === 0 ? "" : `?${params.toString()}`;
  return request<ListComputersResponse>(`${environmentPath(scope)}/computers${query}`);
}

export type ComputerMember = {
  kind: "session" | "task" | "command";
  id: string;
  run_id?: string;
  state: "admitted" | "running" | "waiting" | "parked" | "draining" | "unreconciled";
  created_at: string;
};

export async function listComputerMembers(
  id: string,
  scope: ComputerScope,
  options: { cursor?: string | undefined; limit?: number | undefined } = {},
): Promise<{ members: ComputerMember[]; next_cursor?: string }> {
  const params = new URLSearchParams();
  if (options.cursor) params.set("cursor", options.cursor);
  if (options.limit !== undefined) params.set("limit", String(options.limit));
  return request(`${computerPath(id, scope)}/members${params.size ? `?${params}` : ""}`);
}

export async function getComputer(id: string, scope: ComputerScope): Promise<Computer> {
  return request<Computer>(computerPath(id, scope));
}

export async function createComputer(
  sandboxID: string,
  scope: ComputerScope,
  input: CreateComputerInput,
): Promise<Computer> {
  return postJson<CreateComputerInput, Computer>(
    `${environmentPath(scope)}/sandboxes/${encodeURIComponent(sandboxID)}/computers`,
    input,
  );
}

export async function deleteComputer(
  id: string,
  scope: ComputerScope,
  input: { idempotency_key: string },
): Promise<{ computer_id: string }> {
  return request<{ computer_id: string }>(computerPath(id, scope), {
    method: "DELETE",
    body: JSON.stringify(input),
  });
}

export async function execComputer(
  id: string,
  scope: ComputerScope,
  input: ExecComputerInput,
): Promise<CommandReceipt> {
  return postJson<ExecComputerInput, CommandReceipt>(`${computerPath(id, scope)}/exec`, input);
}

export async function getCommand(commandID: string, scope: ComputerScope, signal?: AbortSignal): Promise<CommandInfo> {
  return request<CommandInfo>(`${environmentPath(scope)}/commands/${encodeURIComponent(commandID)}`, signal ? { signal } : {});
}

export async function listCommandLogs(commandID: string, scope: ComputerScope, cursor?: string, signal?: AbortSignal): Promise<CommandLogPage> {
  const query = new URLSearchParams({ limit: "100" });
  if (cursor) query.set("cursor", cursor);
  return request<CommandLogPage>(`${environmentPath(scope)}/commands/${encodeURIComponent(commandID)}/logs?${query}`, signal ? { signal } : {});
}

export function isTerminalCommandStatus(status: CommandStatus): boolean {
  return ["exited", "failed", "cancelled", "timed_out", "lost"].includes(status);
}

export function decodeCommandLogPage(records: readonly CommandLogRecord[], stream: "stdout" | "stderr", prefix: Uint8Array = new Uint8Array(), end = false): { text: string; pending: Uint8Array } {
  const decoder = new TextDecoder();
  let text = decoder.decode(prefix, { stream: true });
  let tail = prefix;
  for (const record of records) {
    if (record.stream !== stream) continue;
    if (record.kind === "gap") {
      text += decoder.decode() + `\n[Output unavailable: chunks ${record.from_sequence}–${record.through_sequence}]\n`;
      tail = new Uint8Array();
      continue;
    }
    const binary = atob(record.content_base64);
    const bytes = Uint8Array.from(binary, (char) => char.charCodeAt(0));
    text += decoder.decode(bytes, { stream: true });
    // Only a UTF-8 suffix can cross a page boundary; retain at most three bytes.
    const joined = new Uint8Array(tail.length + bytes.length);
    joined.set(tail); joined.set(bytes, tail.length);
    tail = joined.slice(-3);
  }
  if (end) return { text: text + decoder.decode(), pending: new Uint8Array() };
  for (let index = tail.length - 1; index >= 0; index--) {
    const byte = tail[index]!;
    if ((byte & 0xc0) === 0x80) continue;
    const length = byte >= 0xc2 && byte <= 0xdf ? 2 : byte >= 0xe0 && byte <= 0xef ? 3 : byte >= 0xf0 && byte <= 0xf4 ? 4 : 1;
    const suffix = tail.slice(index);
    // TextDecoder remains the validator, including overlong and surrogate bytes.
    if (suffix.length < length && new TextDecoder().decode(suffix, { stream: true }) === "") return { text, pending: suffix };
    break;
  }
  return { text, pending: new Uint8Array() };
}

// The exec form takes argv on one line; arguments are separated by whitespace.
export function parseExecCommand(line: string): string[] {
  return line.split(/\s+/).filter((part) => part !== "");
}

function computerPath(id: string, scope: ComputerScope): string {
  return `${environmentPath(scope)}/computers/${encodeURIComponent(id)}`;
}

function environmentPath(scope: ComputerScope): string {
  if (!scope.projectID || !scope.environmentID) {
    throw new Error("Computer project and environment are required");
  }
  return `/api/projects/${encodeURIComponent(scope.projectID)}/environments/${encodeURIComponent(scope.environmentID)}`;
}
