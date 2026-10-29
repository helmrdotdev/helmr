import { A, useSearchParams } from "@solidjs/router";
import { createQuery, useQueryClient } from "@tanstack/solid-query";
import { createMemo, createSignal, Match, Show, Switch } from "solid-js";
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
    const identity = status.data?.consent;
    if (!identity?.org_id) return;
    const consent = { org_id: identity.org_id };
    setActionError(null);
    setBusy(approve ? "approve" : "deny");
    try {
      const result = approve ? await approveDeviceCode(requestCode, consent,identity.proof) : await denyDeviceCode(requestCode, consent,identity.proof);
      queryClient.setQueryData(["device-code", requestCode], result);
    } catch (error) {
      if (code() !== requestCode) return;
      setActionError(deviceFailure(error, "This device request could not be updated. Try again."));
      const refreshed = await status.refetch();
      if (refreshed.data && refreshed.data.status !== "pending") setActionError(null);
    } finally {
      setBusy(null);
    }
  }

  const canResolve = createMemo(() => status.data?.status === "pending" && !!status.data?.consent && !busy());

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
                <AuthTitle>{device().confirmation ? "Confirm CLI access" : "Authorize CLI"}</AuthTitle>
                <Show when={device().confirmation}><AuthCopy>This replaces the existing CLI login for the same account and organization. Return to your terminal and explicitly retry your command after confirming.</AuthCopy></Show>
                <AuthCopy>Only approve if you started this login. Check that this code matches your terminal.</AuthCopy>
                <div class={ui.authCode} aria-label="Device code">{code()}</div>
                <AuthFacts label="Request">
                  <AuthFact label="Account">{device().consent?.account_name}</AuthFact>
                  <AuthFact label="Organization">{device().consent?.org_name}</AuthFact>
                  <AuthFact label="Device">CLI displaying the code above</AuthFact>
                  <AuthFact label="Access">Your current permissions in this organization</AuthFact>
                  <AuthFact label="Server">{window.location.origin}</AuthFact>
                  <AuthFact label="Expires"><RelativeTime value={device().expires_at} /></AuthFact>
                </AuthFacts>
                <Show when={actionError()}>{(failure) => <AuthError {...failure()} />}</Show>
                <button type="button" class={ui.button} disabled={!canResolve()} onClick={() => resolveDevice(true)}>
                  {busy() === "approve" ? "Approving…" : "Approve"}
                </button>
                <AuthLink disabled={!canResolve()} onClick={() => resolveDevice(false)}>
                  {busy() === "deny" ? "Denying…" : "Deny"}
                </AuthLink>
              </Match>
            </Switch>
          </AuthWindow>
        )}
      </Match>
    </Switch>
  );
}
