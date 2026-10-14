import { A, useNavigate } from "@solidjs/router";
import { createSignal, Match, onCleanup, onMount, Show, Switch } from "solid-js";
import { withNext } from "../lib/continuation";
import { describeFailure, type Failure } from "../lib/error";
import { ApiError } from "../lib/api";
import { confirmSlackUserLink, verifySlackUserLink, type SlackIdentityProof } from "../lib/slack";
import { AuthCopy, AuthError, AuthSuccess, AuthTitle, AuthWindow, AuthWorking } from "../ui/AuthWindow";
import { IDText } from "../ui/IDText";
import { KeyValue, KeyValueList } from "../ui/Panel";
import { ui } from "../ui/styles";

const MESSAGES = {
  bad_request: "The linking request expired, or a different Helmr account is signed in.",
  unauthorized: "Your Helmr sign-in expired before linking finished.",
  forbidden: "This Helmr account cannot link a Slack identity in this organization.",
  conflict: "This Slack account or your Helmr account is already linked differently in this workspace.",
  service_unavailable: "Slack is not configured on this server.",
};

// What to do next, by what went wrong.
function NextStep(props: { failure: Failure & { code: string } }) {
  return <Switch fallback={<>
    <AuthCopy>Use the Helmr account that started linking. If the flow expired, try your action in Slack again or start from My account.</AuthCopy>
    <A class={ui.secondaryButton} href="/settings/account">My account</A>
  </>}>
    <Match when={props.failure.code === "unauthorized"}>
      <AuthCopy>Sign in again, then try your action in Slack again.</AuthCopy>
      <A class={ui.secondaryButton} href={withNext("/login", "/settings/account")}>Sign in again</A>
    </Match>
    <Match when={props.failure.code === "conflict"}>
      <AuthCopy>Unlink the other account from My account, then link again.</AuthCopy>
      <A class={ui.secondaryButton} href="/settings/account">My account</A>
    </Match>
    <Match when={props.failure.code === "forbidden" || props.failure.code === "service_unavailable"}>
      <AuthCopy>Ask an organization owner or admin for help.</AuthCopy>
      <A class={ui.secondaryButton} href="/settings/account">My account</A>
    </Match>
  </Switch>;
}

function linkFailure(error: unknown, fallback: string): Failure & { code: string } {
  return { ...describeFailure(error, MESSAGES, fallback), code: error instanceof ApiError ? error.code : "" };
}

export function AuthSlackLink() {
  const navigate = useNavigate();
  const [proof, setProof] = createSignal<SlackIdentityProof | null>(null);
  const [verifyError, setVerifyError] = createSignal<(Failure & { code: string }) | null>(null);
  const [confirmError, setConfirmError] = createSignal<(Failure & { code: string }) | null>(null);
  const [busy, setBusy] = createSignal(false);
  const [complete, setComplete] = createSignal(false);
  let mounted = true; onCleanup(() => { mounted = false; });
  onMount(async () => {
    const fragment = new URLSearchParams(window.location.hash.slice(1));
    const input = { state: fragment.get("state") ?? "", code: fragment.get("code") ?? "", error: fragment.get("error") ?? "" };
    history.replaceState({}, "", "/auth/slack/link");
    try { const result = await verifySlackUserLink(input); if (mounted) setProof(result); }
    catch (e) { if (mounted) setVerifyError(linkFailure(e, "Your Slack account could not be verified.")); }
  });
  const confirm = async () => {
    const current = proof(); if (!current) return;
    setBusy(true); setConfirmError(null);
    try { await confirmSlackUserLink(current.confirmation); if (mounted) { if (current.return_url) setComplete(true); else navigate("/settings/account", { replace: true }); } }
    catch (e) { if (mounted) setConfirmError(linkFailure(e, "Your accounts could not be linked.")); }
    finally { if (mounted) setBusy(false); }
  };
  return <Switch fallback={<AuthWorking>Verifying your Slack account...</AuthWorking>}>
    <Match when={verifyError()}>
      {(failure) => <AuthWindow><AuthError title="Link your Slack account" message={failure().message} details={failure().details} action={<NextStep failure={failure()} />} /></AuthWindow>}
    </Match>
    <Match when={complete() && proof()}>
      {(current) => (
        <AuthWindow>
          <AuthSuccess title="Your accounts are linked" action={<a class={ui.button} href={current().return_url} rel="noreferrer">Return to Slack</a>}>
            Return to Slack and send a new message or repeat your action. Linking does not apply your earlier action.
          </AuthSuccess>
        </AuthWindow>
      )}
    </Match>
    <Match when={proof()}>
      {(current) => (
        <AuthWindow>
          <AuthTitle>Link your Slack account</AuthTitle>
          <div class={ui.blockStack}>
            <KeyValueList title="Accounts">
              <KeyValue label="Slack account">{current().identity.name || <IDText value={current().identity.slack_user_id} />}</KeyValue>
              <KeyValue label="Slack user ID"><IDText value={current().identity.slack_user_id} /></KeyValue>
              <KeyValue label="Slack workspace">{current().workspace_name || <IDText value={current().identity.team_id} />}</KeyValue>
              <KeyValue label="Helmr account">{current().helmr_display_name}</KeyValue>
              <KeyValue label="Organization">{current().helmr_org_name}</KeyValue>
            </KeyValueList>
            <AuthCopy>Confirm that both accounts are yours. Slack actions will use this Helmr account’s current permissions. Linking does not change organization membership.</AuthCopy>
            <Show when={confirmError()}>{(failure) => <AuthError message={failure().message} details={failure().details} action={<NextStep failure={failure()} />} />}</Show>
            <button type="button" class={ui.button} disabled={busy()} onClick={() => void confirm()}>{busy() ? "Linking..." : "Confirm these accounts are mine"}</button>
            <A href="/settings/account">My account</A>
          </div>
        </AuthWindow>
      )}
    </Match>
  </Switch>;
}
