import { ApiError, del, postJson, request } from "./api";

export type MemberRole = "owner" | "admin" | "developer" | "viewer";
type MemberStatus = "active" | "disabled";
type InvitationStatus = "pending" | "accepted" | "revoked" | "expired";

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

type ListMembersResponse = {
  members: OrganizationMember[];
};

type ListInvitationsResponse = {
  invitations: OrganizationInvitation[];
  next_cursor?: string;
};

// The most addresses one invitation request may name.
export const MAX_INVITATIONS_PER_REQUEST = 50;

type CreateInvitationsInput = {
  emails: string[];
  role: MemberRole;
};

// One address of an invitation request: invited (its invitation was
// emailed), or failed with the reason.
export type InvitationResult = {
  email: string;
  status: "invited" | "failed";
  error?: { code: string; message: string };
};

type CreateInvitationsResponse = {
  results: InvitationResult[];
};

export type InviteEntry = {
  email: string;
  state: "valid" | "invalid" | "duplicate";
};

// A bare address the server accepts: a dot-atom local part and domain (ASCII
// atext or any non-ASCII character), at most 320 characters.
const ATOM = "[A-Za-z0-9!#$%&'*+/=?^_`{|}~\\-\\u0080-\\uFFFF]+";
const DOT_ATOM = `${ATOM}(?:\\.${ATOM})*`;
const ADDRESS = new RegExp(`^${DOT_ATOM}@${DOT_ATOM}$`);

export function validInviteAddress(address: string): boolean {
  return address.length <= 320 && ADDRESS.test(address);
}

// The addresses typed or pasted into the invite form, lowercased, in order.
// Entries are separated by commas, semicolons, spaces or line breaks; a
// mail-client entry such as `Ada Lovelace <ada@example.com>` or
// `"Lovelace, Ada" <ada@example.com>` stands for the address in angle
// brackets. A repeat of an earlier address is a duplicate; only valid entries
// are sent.
export function parseInviteEmails(text: string): InviteEntry[] {
  const addresses: string[] = [];
  // Words not yet known to be a display name: they are dropped when an
  // `<address>` follows them, and are entries of their own otherwise.
  let pending: string[] = [];
  const flush = () => {
    addresses.push(...pending);
    pending = [];
  };
  const tokens = text.replace(/"[^"]*"/g, " ").split(/([,;\n])|[^\S\n]+/);
  for (const token of tokens) {
    if (token === undefined || token === "") continue;
    if (token === "," || token === ";" || token === "\n") {
      flush();
      continue;
    }
    const bracketed = /<([^<>]*)>/.exec(token);
    if (bracketed) {
      const name = token.slice(0, bracketed.index);
      pending = [];
      if (name.includes("@")) addresses.push(name);
      addresses.push(bracketed[1]!.trim());
      continue;
    }
    if (token.includes("@") || token.includes("<") || token.includes(">")) {
      flush();
      addresses.push(token);
      continue;
    }
    pending.push(token);
  }
  flush();
  const seen = new Set<string>();
  return addresses.map((address) => {
    const email = address.toLowerCase();
    if (seen.has(email)) return { email, state: "duplicate" as const };
    seen.add(email);
    return { email, state: validInviteAddress(email) ? ("valid" as const) : ("invalid" as const) };
  });
}

export function memberResourceID(member: OrganizationMember): string {
  return member.user_id;
}

export function invitationResourceID(invitation: OrganizationInvitation): string {
  return invitation.id;
}

export async function listMembers(): Promise<ListMembersResponse> {
  return request<ListMembersResponse>("/api/members");
}

export async function listInvitations(cursor?: string): Promise<ListInvitationsResponse> {
  const query = cursor ? `?cursor=${encodeURIComponent(cursor)}` : "";
  return request<ListInvitationsResponse>(`/api/invitations${query}`);
}

export async function createInvitations(input: CreateInvitationsInput): Promise<InvitationResult[]> {
  const response = await postJson<CreateInvitationsInput, CreateInvitationsResponse>("/api/invitations", input);
  return response.results;
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
