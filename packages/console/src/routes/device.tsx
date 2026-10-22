import { ME_QUERY_KEY, useMe } from "../lib/me";
import { A, useSearchParams } from "@solidjs/router";
import { createQuery, useQueryClient } from "@tanstack/solid-query";
import { createMemo, createSignal, Match, Show, Switch } from "solid-js";
import { ApiError } from "../lib/api";
import { approveDeviceCode, denyDeviceCode, getDeviceCodeStatus } from "../lib/auth";
import { AUTH_MESSAGES, describeFailure, type Failure } from "../lib/error";
import { AuthCopy, AuthError, AuthFact, AuthFacts, AuthLink, AuthMissing, AuthSuccess, AuthTitle, AuthWindow } from "../ui/AuthWindow";
import { RelativeTime } from "../ui/RelativeTime";
import { cx, ui } from "../ui/styles";

function readParam(value: string | string[] | undefined): string {
  return Array.isArray(value) ? value[0] ?? "" : value ?? "";
}

function normalizeCode(value: string): string {
  return value.trim().toUpperCase();
}

const DEVICE_MESSAGES = {
  ...AUTH_MESSAGES,
  not_found: "This device request was not found. It may have expired or already been used.",
  device_identity_changed: "Your account changed since this page loaded. Review the account and organization, then approve again.",
};

const deviceFailure = (error: unknown, fallback: string): Failure => describeFailure(error, DEVICE_MESSAGES, fallback);

function RestartLine() {
  return <>Run <code>helmr login</code> again in your terminal.</>;
}

function RestartHint() {
  return <AuthCopy><RestartLine /></AuthCopy>;
}

export function Device() {
  const [params] = useSearchParams();
  const queryClient = useQueryClient();
  const me = useMe();
  const code = createMemo(() => normalizeCode(readParam(params["code"])));
  const [busy, setBusy] = createSignal<"approve" | "deny" | null>(null);
  const [actionError, setActionError] = createSignal<Failure | null>(null);

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
      setActionError(deviceFailure(error, "This device request could not be updated. Try again."));
      if (error instanceof ApiError && error.code === "device_identity_changed") {
        await queryClient.invalidateQueries({ queryKey: ME_QUERY_KEY });
      }
      const refreshed = await status.refetch();
      if (refreshed.data && refreshed.data.status !== "pending") setActionError(null);
    } finally {
      setBusy(null);
    }
  }

  const canResolve = createMemo(() => status.data?.status === "pending" && !!me.data?.org_id && !me.isError && !busy());

  return (
    <Switch>
      <Match when={code() === ""}>
        <AuthWindow>
          <AuthMissing title="This approval link is incomplete" action={<A class={cx(ui.button, "auth-full")} href="/">Go to the console</A>}>
            The link is missing the device code from your terminal. Run <code>helmr login</code> again in your terminal and open the link it shows.
          </AuthMissing>
        </AuthWindow>
      </Match>
      <Match when={status.isError}>
        <AuthWindow>
          <AuthError
            title="Authorize CLI"
            {...deviceFailure(status.error, "This device request could not be loaded.")}
            action={<RestartHint />}
          />
        </AuthWindow>
      </Match>
      <Match when={status.data}>
        {(device) => (
          <AuthWindow>
            <Switch>
              <Match when={device().status === "approved"}>
                <AuthSuccess title="CLI authorized">Return to your terminal to finish signing in.</AuthSuccess>
              </Match>
              <Match when={device().status === "denied"}>
                <AuthSuccess title="CLI sign-in denied"><RestartLine /></AuthSuccess>
              </Match>
              <Match when={device().status === "expired"}>
                <AuthSuccess title="This request expired"><RestartLine /></AuthSuccess>
              </Match>
              <Match when={device().status === "consumed"}>
                <AuthSuccess title="This request was already used">Check your terminal for the login result.</AuthSuccess>
              </Match>
              <Match when={device().status === "pending"}>
                <AuthTitle>Authorize CLI</AuthTitle>
                <AuthCopy>Only approve if you started this login. Check that this code matches your terminal.</AuthCopy>
                <div class={ui.authCode} aria-label="Device code">{code()}</div>
                <AuthFacts label="Request">
                  <AuthFact label="Account">{me.data?.display_name || me.data?.user_id}</AuthFact>
                  <AuthFact label="Organization">{me.data?.org_name || me.data?.org_id}</AuthFact>
                  <AuthFact label="Server">{me.data?.app_origin || window.location.origin}</AuthFact>
                  <AuthFact label="Expires"><RelativeTime value={device().expires_at} /></AuthFact>
                </AuthFacts>
                <Show when={actionError()}>{(failure) => <AuthError {...failure()} />}</Show>
                <button type="button" class={ui.button} disabled={!canResolve()} onClick={() => resolveDevice(true)}>
                  {busy() === "approve" ? "Approving..." : "Approve"}
                </button>
                <AuthLink disabled={!canResolve()} onClick={() => resolveDevice(false)}>
                  {busy() === "deny" ? "Denying..." : "Deny"}
                </AuthLink>
              </Match>
            </Switch>
          </AuthWindow>
        )}
      </Match>
    </Switch>
  );
}
