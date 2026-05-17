import { useSearchParams } from "@solidjs/router";
import { createQuery, useQueryClient } from "@tanstack/solid-query";
import { createMemo, createSignal, Match, Show, Switch } from "solid-js";
import { ApiError } from "../lib/api";
import {
  approveDeviceCode,
  denyDeviceCode,
  getDeviceCodeStatus,
  type DeviceCodeStatus,
} from "../lib/auth";
import { errorMessage } from "../lib/error";
import { AuthCopy, AuthScreen, AuthTitle } from "../ui/AuthScreen";
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
  const kind = error instanceof ApiError ? error.errorKind : null;
  return errorMessage(kind, error instanceof Error ? error.message : fallback);
}

export function Device() {
  const [params] = useSearchParams();
  const queryClient = useQueryClient();
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
    setActionError(null);
    setBusy(approve ? "approve" : "deny");
    try {
      const result = approve ? await approveDeviceCode(code()) : await denyDeviceCode(code());
      queryClient.setQueryData(["device-code", code()], result);
    } catch (error) {
      setActionError(deviceErrorMessage(error, "Could not update this device code."));
    } finally {
      setBusy(null);
    }
  }

  const current = createMemo(() => status.data);
  const canResolve = createMemo(() => current()?.status === "pending" && !busy());

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
              <AuthCopy>Review the code shown in your terminal before approving.</AuthCopy>
              <div class={ui.authCode} aria-label="Device code">{code()}</div>
              <p class={ui.authStatus}>Status: {statusText(device().status)}</p>
              <Show when={device().expires_at}>
                <p class={ui.muted}>Expires at {new Date(device().expires_at ?? "").toLocaleString()}</p>
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
