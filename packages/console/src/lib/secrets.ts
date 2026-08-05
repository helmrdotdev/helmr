import { request } from "./api";

export type Secret = {
  id: string;
  name: string;
  status: "active" | "revoked";
  created_at: string;
  rotated_at?: string;
  revoked_at?: string;
};

export type ListSecretsResponse = {
  secrets: Secret[];
};

export async function listSecrets(projectID: string, environmentID: string): Promise<ListSecretsResponse> {
  return request<ListSecretsResponse>(secretsPath(projectID, environmentID));
}

export async function createSecret(
  name: string,
  value: string,
  projectID: string,
  environmentID: string,
): Promise<Secret> {
  return request<Secret>(secretsPath(projectID, environmentID), {
    method: "POST",
    body: JSON.stringify({
      name,
      value,
      idempotency_key: crypto.randomUUID(),
    }),
  });
}

export async function rotateSecret(
  id: string,
  value: string,
  projectID: string,
  environmentID: string,
): Promise<Secret> {
  return request<Secret>(`${secretsPath(projectID, environmentID)}/${encodeURIComponent(id)}/rotate`, {
    method: "POST",
    body: JSON.stringify({
      value,
      idempotency_key: crypto.randomUUID(),
    }),
  });
}

export async function revokeSecret(id: string, projectID: string, environmentID: string): Promise<Secret> {
  return request<Secret>(`${secretsPath(projectID, environmentID)}/${encodeURIComponent(id)}/revoke`, {
    method: "POST",
    body: JSON.stringify({
      idempotency_key: crypto.randomUUID(),
    }),
  });
}

function secretsPath(projectID: string, environmentID: string): string {
  return `/api/projects/${encodeURIComponent(projectID)}/environments/${encodeURIComponent(environmentID)}/secrets`;
}
