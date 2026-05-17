import { postJson, request } from "./api";

export type Me = {
  user_id: string;
  display_name: string | null;
  profile_image_url: string;
  org_id: string;
  role: string;
  permissions: string[];
};

export async function getMe(): Promise<Me> {
  return request<Me>("/api/me");
}

export type BootstrapStatus = {
  setup_enabled: boolean;
  bootstrap_required: boolean;
  bootstrap_owner_email_configured?: boolean;
};

export async function getBootstrapStatus(): Promise<BootstrapStatus> {
  return request<BootstrapStatus>("/api/bootstrap/status", { redirectOnUnauthorized: false });
}

export type MagicLinkStartResponse = {
  sent: true;
  debug_url?: string;
};

export type InviteMagicLinkStartResponse = MagicLinkStartResponse & {
  email?: string;
};

export async function startMagicLinkLogin(input: {
  email: string;
  next?: string;
}): Promise<MagicLinkStartResponse> {
  return postJson<{ email: string; next?: string }, MagicLinkStartResponse>(
    "/api/auth/magic-link/start",
    input.next ? input : { email: input.email },
    { redirectOnUnauthorized: false },
  );
}

export async function startInviteMagicLink(token: string): Promise<InviteMagicLinkStartResponse> {
  return postJson<{ token: string }, InviteMagicLinkStartResponse>(
    "/api/auth/magic-link/invite/start",
    { token },
    { redirectOnUnauthorized: false },
  );
}

export async function finishMagicLink(token: string): Promise<{ redirect_after: string }> {
  return postJson<{ token: string }, { redirect_after: string }>(
    "/api/auth/magic-link/finish",
    { token },
    { redirectOnUnauthorized: false },
  );
}

export async function startGitHubLogin(next?: string): Promise<{ redirect_url: string }> {
  return postJson<{ next?: string }, { redirect_url: string }>(
    "/api/auth/github/start",
    next ? { next } : {},
    { redirectOnUnauthorized: false },
  );
}

export async function startGitHubInvite(token: string): Promise<{ redirect_url: string }> {
  return postJson<{ token: string }, { redirect_url: string }>(
    "/api/auth/github/invite/start",
    { token },
    { redirectOnUnauthorized: false },
  );
}

export async function finishGitHubAuth(input: {
  code: string;
  state: string;
  error?: string;
  error_description?: string;
}): Promise<{ redirect_after: string }> {
  return postJson<
    {
      code: string;
      state: string;
      error?: string;
      error_description?: string;
    },
    { redirect_after: string }
  >("/api/auth/github/finish", input, { redirectOnUnauthorized: false });
}

export async function logout(): Promise<void> {
  await postJson<Record<string, never>, void>("/api/auth/logout", {});
  window.location.href = "/login";
}

export type DeviceCodeStatus = {
  status: "pending" | "approved" | "denied" | "consumed" | "expired";
  expires_at?: string;
};

export async function getDeviceCodeStatus(userCode: string): Promise<DeviceCodeStatus> {
  return request<DeviceCodeStatus>(
    `/api/auth/device/status?user_code=${encodeURIComponent(userCode)}`,
  );
}

export async function approveDeviceCode(userCode: string): Promise<DeviceCodeStatus> {
  return postJson<{ user_code: string }, DeviceCodeStatus>("/api/auth/device/approve", {
    user_code: userCode,
  });
}

export async function denyDeviceCode(userCode: string): Promise<DeviceCodeStatus> {
  return postJson<{ user_code: string }, DeviceCodeStatus>("/api/auth/device/deny", {
    user_code: userCode,
  });
}
