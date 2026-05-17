import { ApiError, del, postJson, request } from "./api";

export type MemberRole = "owner" | "admin" | "developer" | "viewer";
export type MemberStatus = "active" | "disabled" | "pending";
export type InvitationStatus = "pending" | "accepted" | "revoked" | "expired";

export type OrganizationMember = {
  user_id: string;
  display_name: string;
  email?: string | null;
  role: MemberRole;
  status: MemberStatus;
  created_at: string;
  updated_at: string;
  disabled_at?: string | null;
};

export type OrganizationInvitation = {
  id: string;
  email: string;
  role: MemberRole;
  status: InvitationStatus;
  created_at: string;
  expires_at: string;
};

export type ListMembersResponse = {
  members: OrganizationMember[];
};

export type ListInvitationsResponse = {
  invitations: OrganizationInvitation[];
};

export type CreateInvitationInput = {
  email: string;
  role: MemberRole;
};

export type CreatedInvitation = {
  invitation: OrganizationInvitation;
  invite_url: string;
};

export function memberResourceID(member: OrganizationMember): string {
  return member.user_id;
}

export function invitationResourceID(invitation: OrganizationInvitation): string {
  return invitation.id;
}

export async function listMembers(): Promise<ListMembersResponse> {
  return request<ListMembersResponse>("/api/members");
}

export async function listInvitations(): Promise<ListInvitationsResponse> {
  return request<ListInvitationsResponse>("/api/invitations");
}

export async function createInvitation(input: CreateInvitationInput): Promise<CreatedInvitation> {
  const response = await postJson<CreateInvitationInput, OrganizationInvitation & { invite_url: string }>(
    "/api/invitations",
    input,
  );
  const { invite_url, ...invitation } = response;
  return {
    invitation,
    invite_url,
  };
}

export async function updateMemberRole(id: string, role: MemberRole, expectedRole: MemberRole): Promise<OrganizationMember> {
  return request<OrganizationMember>(`/api/members/${encodeURIComponent(id)}`, {
    method: "PATCH",
    body: JSON.stringify({ role, expected_role: expectedRole }),
  });
}

export async function removeMember(id: string): Promise<void> {
  try {
    await del<Record<string, never>>(`/api/members/${encodeURIComponent(id)}`);
  } catch (error) {
    if (error instanceof ApiError && error.status === 404) return;
    throw error;
  }
}

export async function revokeInvitation(id: string): Promise<void> {
  try {
    await del<Record<string, never>>(`/api/invitations/${encodeURIComponent(id)}`);
  } catch (error) {
    if (error instanceof ApiError && error.status === 404) return;
    throw error;
  }
}
