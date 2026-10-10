import { request } from "./api";

export type ComputerSecret =
  | { secretId: string; env: { name: string; mode: "raw"; allowedOrigins?: never } | { name: string; mode: "protected"; allowedOrigins: string[] }; file?: never }
  | { secretId: string; file: { path: string }; env?: never };

export type ComputerStatus = "available" | "deleted" | "deleting";

export type Computer = {
  id: string;
  key?: string;
  definition_key: string;
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
  kind: "session" | "command";
  id: string;
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

function computerPath(id: string, scope: ComputerScope): string {
  return `${environmentPath(scope)}/computers/${encodeURIComponent(id)}`;
}

function environmentPath(scope: ComputerScope): string {
  if (!scope.projectID || !scope.environmentID) {
    throw new Error("Computer project and environment are required");
  }
  return `/api/projects/${encodeURIComponent(scope.projectID)}/environments/${encodeURIComponent(scope.environmentID)}`;
}
