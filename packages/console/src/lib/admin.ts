import { postJson, request } from "./api";

export type AdminRegion = {
  id: string;
  display_name: string;
  location: string;
};

export type CreateAdminRegionInput = AdminRegion;

export type AdminWorkerGroup = {
  id: string;
  region_id: string;
  name: string;
  description: string;
  state: "active" | "paused" | "draining" | "disabled";
  claim_version: number;
  allows_run: boolean;
  allows_build: boolean;
  required_cpu_millis: number;
  required_memory_bytes: number;
  required_guest_ephemeral_disk_bytes: number;
  required_build_cache_bytes: number;
  required_artifact_cache_bytes: number;
  required_vm_slots: number;
};

export type CreateAdminWorkerGroupInput = Omit<AdminWorkerGroup, "id" | "state" | "claim_version">;

export async function listAdminRegions(): Promise<{ regions: AdminRegion[] }> {
  return request("/admin/api/v1/regions");
}

export async function createAdminRegion(input: CreateAdminRegionInput): Promise<AdminRegion> {
  return postJson("/admin/api/v1/regions", input);
}

export async function updateAdminRegion(
  id: string,
  input: Pick<AdminRegion, "display_name" | "location">,
): Promise<AdminRegion> {
  return request(`/admin/api/v1/regions/${encodeURIComponent(id)}`, {
    method: "PATCH",
    body: JSON.stringify(input),
  });
}

export async function listAdminWorkerGroups(regionID = ""): Promise<{ worker_groups: AdminWorkerGroup[] }> {
  const query = regionID ? `?region_id=${encodeURIComponent(regionID)}` : "";
  return request(`/admin/api/v1/worker-groups${query}`);
}

export async function createAdminWorkerGroup(input: CreateAdminWorkerGroupInput): Promise<{
  worker_group: AdminWorkerGroup;
  enrollment_token: string;
}> {
  return postJson("/admin/api/v1/worker-groups", input);
}

export async function updateAdminWorkerGroup(id: string, description: string): Promise<AdminWorkerGroup> {
  return request(`/admin/api/v1/worker-groups/${encodeURIComponent(id)}`, {
    method: "PATCH",
    body: JSON.stringify({ description }),
  });
}

export async function transitionAdminWorkerGroup(
  group: AdminWorkerGroup,
  action: "pause" | "activate" | "drain" | "disable",
): Promise<{ id: string; state: AdminWorkerGroup["state"]; claim_version: number }> {
  return postJson(`/admin/api/v1/worker-groups/${encodeURIComponent(group.id)}/${action}`, {
    expected_claim_version: group.claim_version,
  });
}

export async function rotateAdminWorkerGroupToken(id: string): Promise<{ enrollment_token: string }> {
  return postJson(`/admin/api/v1/worker-groups/${encodeURIComponent(id)}/token/rotate`, {});
}
