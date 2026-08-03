import { request } from "./api";

export type Secret = {
  id: string;
  name: string;
  state: "active" | "revoked";
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

export async function setSecret(
  name: string,
  value: string,
  projectID: string,
  environmentID: string,
): Promise<Secret> {
  return request<Secret>(`${secretsPath(projectID, environmentID)}/${encodeURIComponent(name)}`, {
    method: "PUT",
    body: JSON.stringify({
      value,
    }),
  });
}

export async function revokeSecret(name: string, projectID: string, environmentID: string): Promise<Secret> {
  return request<Secret>(`${secretsPath(projectID, environmentID)}/${encodeURIComponent(name)}/revoke`, {
    method: "POST",
    body: JSON.stringify({
      idempotency_key: crypto.randomUUID(),
    }),
  });
}

function secretsPath(projectID: string, environmentID: string): string {
  return `/api/projects/${encodeURIComponent(projectID)}/environments/${encodeURIComponent(environmentID)}/secrets`;
}
