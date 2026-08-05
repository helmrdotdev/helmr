import { request } from "./api";

export type SessionStatus = "open" | "closed" | "cancelled" | "failed";

export type Session = {
  id: string;
  actor_id: string;
  deployment_id: string;
  key?: string;
  status: SessionStatus;
  created_at: string;
  updated_at: string;
  current_run_id?: string;
  failure?: {
    code: string;
    message: string;
    details: { run_id?: string };
  };
};

export type SessionOutputRecord = {
  id: string;
  sequence: number;
  data: unknown;
  content_type: string;
  created_at: string;
  provenance: {
    run_id: string;
    attempt_number: number;
    deployment_id: string;
  };
};

export type SessionOutputPage = {
  records: SessionOutputRecord[];
  next_after: number;
  has_more: boolean;
};

export type SessionAddress = {
  sessionID: string;
  projectID: string;
  environmentID: string;
};

export async function getSession(address: SessionAddress): Promise<Session> {
  return request<Session>(sessionAPIPath(address));
}

export async function getSessionOutput(
  address: SessionAddress,
  options: { after?: number; limit?: number } = {},
): Promise<SessionOutputPage> {
  const params = new URLSearchParams();
  if (options.after !== undefined) params.set("after", String(options.after));
  if (options.limit !== undefined) params.set("limit", String(options.limit));
  const query = params.size === 0 ? "" : `?${params.toString()}`;
  return request<SessionOutputPage>(`${sessionAPIPath(address)}/outputs${query}`);
}

export function sessionConsolePath(
  sessionID: string,
  projectID: string,
  environmentID: string,
): string {
  const params = new URLSearchParams({
    project_id: projectID,
    environment_id: environmentID,
  });
  return `/sessions/${encodeURIComponent(sessionID)}?${params.toString()}`;
}

export function runSessionConsolePath(
  run: { session_id?: string },
  projectID: string,
  environmentID: string,
): string | undefined {
  if (!run.session_id) return undefined;
  return sessionConsolePath(run.session_id, projectID, environmentID);
}

function sessionAPIPath(address: SessionAddress): string {
  if (!address.projectID || !address.environmentID) {
    throw new Error("Session project and environment are required");
  }
  return `/api/projects/${encodeURIComponent(address.projectID)}/environments/${encodeURIComponent(address.environmentID)}/sessions/${encodeURIComponent(address.sessionID)}`;
}
