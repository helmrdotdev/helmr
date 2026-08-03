import { request } from "./api";

export type ActorStatus = "open" | "closed" | "cancelled" | "failed";

export type ActorSnapshot = {
  id: string;
  key?: string;
  status: ActorStatus;
  created_at: string;
  updated_at: string;
  current_run_id?: string;
  failure?: {
    code: string;
    run_id: string;
  };
};

export type ActorOutputRecord = {
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

export type ActorOutputPage = {
  records: ActorOutputRecord[];
  next_after: number;
  has_more: boolean;
};

export type ActorAddress = {
  declaredID: string;
  actorID: string;
  projectID: string;
  environmentID: string;
};

export async function getActor(address: ActorAddress): Promise<ActorSnapshot> {
  return request<ActorSnapshot>(`${actorPath(address)}/status?${actorQuery(address.actorID)}`);
}

export async function getActorOutput(
  address: ActorAddress,
  options: { after?: number; limit?: number } = {},
): Promise<ActorOutputPage> {
  const params = new URLSearchParams(actorQuery(address.actorID));
  if (options.after !== undefined) params.set("after", String(options.after));
  if (options.limit !== undefined) params.set("limit", String(options.limit));
  return request<ActorOutputPage>(`${actorPath(address)}/output?${params.toString()}`);
}

export function actorConsolePath(
  declaredID: string,
  actorID: string,
  projectID: string,
  environmentID: string,
): string {
  const params = new URLSearchParams({
    project_id: projectID,
    environment_id: environmentID,
  });
  return `/actors/${encodeURIComponent(declaredID)}/${encodeURIComponent(actorID)}?${params.toString()}`;
}

export function actorRunConsolePath(
  run: {
    entrypoint: { kind: string; id: string };
    actor_id?: string;
  },
  projectID: string,
  environmentID: string,
): string | undefined {
  if (run.entrypoint.kind !== "actor" || !run.actor_id) return undefined;
  return actorConsolePath(
    run.entrypoint.id,
    run.actor_id,
    projectID,
    environmentID,
  );
}

function actorPath(address: ActorAddress): string {
  if (!address.projectID || !address.environmentID) {
    throw new Error("project and environment are required");
  }
  return `/api/projects/${encodeURIComponent(address.projectID)}/environments/${encodeURIComponent(address.environmentID)}/actors/${encodeURIComponent(address.declaredID)}`;
}

function actorQuery(actorID: string): string {
  return new URLSearchParams({ actor_id: actorID }).toString();
}
