import { postJson, request } from "./api";

export type WaitpointPolicyDelivery = {
  type: "email" | string;
  to?: string[];
};

export type WaitpointPolicy = {
  id: string;
  project_id: string;
  environment_id: string;
  name: string;
  label: string;
  config: {
    deliveries?: WaitpointPolicyDelivery[];
    resolution?: { type?: string; count?: number };
  };
  created_at: string;
  updated_at: string;
};

export type ListWaitpointPoliciesResponse = {
  policies: WaitpointPolicy[];
};

export type SaveWaitpointPolicyInput = {
  projectID: string;
  environmentID: string;
  name: string;
  label?: string;
  recipients: string[];
};

export async function listWaitpointPolicies(projectID: string, environmentID: string): Promise<ListWaitpointPoliciesResponse> {
  return request<ListWaitpointPoliciesResponse>(waitpointPoliciesPath(projectID, environmentID));
}

export async function createWaitpointPolicy(input: SaveWaitpointPolicyInput): Promise<WaitpointPolicy> {
  return postJson<WaitpointPolicyRequest, WaitpointPolicy>(
    waitpointPoliciesPath(input.projectID, input.environmentID),
    waitpointPolicyRequest(input, true),
  );
}

export async function updateWaitpointPolicy(name: string, input: Omit<SaveWaitpointPolicyInput, "name">): Promise<WaitpointPolicy> {
  return request<WaitpointPolicy>(waitpointPolicyPath(name, input.projectID, input.environmentID), {
    method: "PATCH",
    body: JSON.stringify(waitpointPolicyRequest({ name, ...input }, false)),
  });
}

export async function deleteWaitpointPolicy(name: string, projectID: string, environmentID: string): Promise<void> {
  return request<void>(waitpointPolicyPath(name, projectID, environmentID), { method: "DELETE" });
}

export function waitpointPolicyRecipients(policy: WaitpointPolicy): string[] {
  return (policy.config.deliveries ?? [])
    .filter((delivery) => delivery.type === "email")
    .flatMap((delivery) => delivery.to ?? []);
}

type WaitpointPolicyRequest = {
  name?: string;
  label?: string;
  config: {
    deliveries: [{ type: "email"; to: string[] }];
    resolution: { type: "any"; count: 1 };
  };
};

function waitpointPolicyRequest(input: SaveWaitpointPolicyInput, includeName: boolean): WaitpointPolicyRequest {
  return {
    ...(includeName ? { name: input.name } : {}),
    ...(input.label === undefined ? {} : { label: input.label }),
    config: {
      deliveries: [{ type: "email", to: input.recipients }],
      resolution: { type: "any", count: 1 },
    },
  };
}

function waitpointPoliciesPath(projectID: string, environmentID: string): string {
  return `/api/projects/${encodeURIComponent(projectID)}/environments/${encodeURIComponent(environmentID)}/waitpoint-policies`;
}

function waitpointPolicyPath(name: string, projectID: string, environmentID: string): string {
  return `${waitpointPoliciesPath(projectID, environmentID)}/${encodeURIComponent(name)}`;
}
