import { useSearchParams } from "@solidjs/router";
import { createQuery, useQueryClient } from "@tanstack/solid-query";
import { createMemo, createSignal, Match, Show, Switch } from "solid-js";
import { ApiError } from "../lib/api";
import {
  approveDeviceCode,
  denyDeviceCode,
  getDeviceCodeStatus,
  getMe,
  type DeviceCodeStatus,
} from "../lib/auth";
import { errorMessage } from "../lib/error";
import { AuthCopy, AuthScreen, AuthTitle } from "../ui/AuthScreen";
import { RelativeTime } from "../ui/RelativeTime";
import { ui } from "../ui/styles";

function readParam(value: string | string[] | undefined): string {
  return Array.isArray(value) ? value[0] ?? "" : value ?? "";
}

function normalizeCode(value: string): string {
  return value.trim().toUpperCase();
}

function statusText(status: DeviceCodeStatus["status"]): string {
  switch (status) {
    case "pending":
      return "Waiting for approval";
    case "approved":
      return "Approved";
    case "denied":
      return "Denied";
    case "consumed":
      return "Already used";
    case "expired":
      return "Expired";
  }
}

function deviceErrorMessage(error: unknown, fallback: string): string {
  const kind = error instanceof ApiError ? error.code : null;
  return errorMessage(kind, error instanceof Error ? error.message : fallback);
}

export function Device() {
  const [params] = useSearchParams();
  const queryClient = useQueryClient();
  const me = createQuery(() => ({ queryKey: ["me"], queryFn: getMe, retry: false, staleTime: 60_000 }));
  const code = createMemo(() => normalizeCode(readParam(params["code"])));
  const [busy, setBusy] = createSignal<"approve" | "deny" | null>(null);
  const [actionError, setActionError] = createSignal<string | null>(null);

  const status = createQuery(() => ({
    queryKey: ["device-code", code()],
    queryFn: () => getDeviceCodeStatus(code()),
    enabled: code() !== "",
    retry: false,
  }));

  async function resolveDevice(approve: boolean) {
    const requestCode = code();
    const identity = me.data;
    if (!identity?.org_id) return;
    const consent = { user_id: identity.user_id, org_id: identity.org_id };
    setActionError(null);
    setBusy(approve ? "approve" : "deny");
    try {
      const result = approve ? await approveDeviceCode(requestCode, consent) : await denyDeviceCode(requestCode, consent);
      queryClient.setQueryData(["device-code", requestCode], result);
    } catch (error) {
      if (code() !== requestCode) return;
      setActionError(deviceErrorMessage(error, "Could not update this device code."));
      if (error instanceof ApiError && error.code === "device_identity_changed") {
        await queryClient.invalidateQueries({ queryKey: ["me"] });
      }
      const refreshed = await status.refetch();
      if (refreshed.data && refreshed.data.status !== "pending") setActionError(null);
    } finally {
      setBusy(null);
    }
  }

  const current = createMemo(() => status.data);
  const canResolve = createMemo(() => current()?.status === "pending" && !!me.data?.org_id && !me.isError && !busy());

  return (
    <AuthScreen>
      <AuthTitle>Authorize CLI</AuthTitle>
      <Switch>
        <Match when={code() === ""}>
          <p class={ui.error}>Missing device code.</p>
        </Match>
        <Match when={status.isPending}>
          <AuthCopy>Loading device request...</AuthCopy>
        </Match>
        <Match when={status.isError}>
          <p class={ui.error}>
            {deviceErrorMessage(status.error, "This device code could not be loaded.")}
          </p>
        </Match>
        <Match when={current()}>
          {(device) => (
            <>
              <Show when={device().status === "pending"}>
                <AuthCopy>Only approve if you started this login. Check that this code matches your terminal.</AuthCopy>
                <p class={ui.authStatus}>Account: {me.data?.display_name || me.data?.user_id}</p>
                <p class={ui.authStatus}>Organization: {me.data?.org_name || me.data?.org_id}</p>
                <p class={ui.muted}>Server: {me.data?.public_url || window.location.origin}</p>
              </Show>
              <div class={ui.authCode} aria-label="Device code">{code()}</div>
              <p class={ui.authStatus}>Status: {statusText(device().status)}</p>
              <Show when={device().status === "approved"}>
                <AuthCopy>Approved. Return to your terminal to finish signing in.</AuthCopy>
              </Show>
              <Show when={device().status === "consumed"}>
                <AuthCopy>This request has already been used. Check your terminal for the login result.</AuthCopy>
              </Show>
              <Show when={device().status === "denied" || device().status === "expired"}>
                <AuthCopy>Run helmr login again in your terminal to start a new request.</AuthCopy>
              </Show>
              <Show when={device().status === "pending" && device().expires_at}>
                <p class={ui.muted}>Expires <RelativeTime value={device().expires_at} /></p>
              </Show>
              <Show when={device().status === "pending"}>
                <div class={ui.actionRow}>
                  <button
                    class={ui.button}
                    type="button"
                    disabled={!canResolve()}
                    onClick={() => resolveDevice(true)}
                  >
                    {busy() === "approve" ? "Approving..." : "Approve"}
                  </button>
                  <button
                    type="button"
                    class={ui.secondaryButton}
                    disabled={!canResolve()}
                    onClick={() => resolveDevice(false)}
                  >
                    {busy() === "deny" ? "Denying..." : "Deny"}
                  </button>
                </div>
              </Show>
              <Show when={actionError()}>
                <p class={ui.error}>{actionError()}</p>
              </Show>
            </>
          )}
        </Match>
      </Switch>
    </AuthScreen>
  );
}
