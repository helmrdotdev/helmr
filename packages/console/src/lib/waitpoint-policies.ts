import { postJson, request } from "./api";

export type WaitpointPolicyDelivery = {
  type: "email" | string;
  to?: string[];
};

export type WaitpointPolicy = {
  id: string;
  name: string;
  label: string;
  mode: "capability" | string;
  config: {
    mode?: string;
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
  name: string;
  label?: string;
  recipients: string[];
};

export async function listWaitpointPolicies(): Promise<ListWaitpointPoliciesResponse> {
  return request<ListWaitpointPoliciesResponse>("/api/waitpoint-policies");
}

export async function createWaitpointPolicy(input: SaveWaitpointPolicyInput): Promise<WaitpointPolicy> {
  return postJson<WaitpointPolicyRequest, WaitpointPolicy>("/api/waitpoint-policies", waitpointPolicyRequest(input, true));
}

export async function updateWaitpointPolicy(name: string, input: Omit<SaveWaitpointPolicyInput, "name">): Promise<WaitpointPolicy> {
  return request<WaitpointPolicy>(`/api/waitpoint-policies/${encodeURIComponent(name)}`, {
    method: "PATCH",
    body: JSON.stringify(waitpointPolicyRequest({ name, ...input }, false)),
  });
}

export async function disableWaitpointPolicy(name: string): Promise<void> {
  return postJson<Record<string, never>, void>(`/api/waitpoint-policies/${encodeURIComponent(name)}/disable`, {});
}

export function waitpointPolicyRecipients(policy: WaitpointPolicy): string[] {
  return (policy.config.deliveries ?? [])
    .filter((delivery) => delivery.type === "email")
    .flatMap((delivery) => delivery.to ?? []);
}

type WaitpointPolicyRequest = {
  name?: string;
  label?: string;
  mode: "capability";
  config: {
    mode: "capability";
    deliveries: [{ type: "email"; to: string[] }];
    resolution: { type: "any"; count: 1 };
  };
};

function waitpointPolicyRequest(input: SaveWaitpointPolicyInput, includeName: boolean): WaitpointPolicyRequest {
  return {
    ...(includeName ? { name: input.name } : {}),
    ...(input.label === undefined ? {} : { label: input.label }),
    mode: "capability",
    config: {
      mode: "capability",
      deliveries: [{ type: "email", to: input.recipients }],
      resolution: { type: "any", count: 1 },
    },
  };
}
