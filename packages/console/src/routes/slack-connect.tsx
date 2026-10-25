import { A, useLocation } from "@solidjs/router";
import { createSignal, Match, onCleanup, onMount, Show, Switch } from "solid-js";
import { ApiError, postJson } from "../lib/api";
import { locationDestination, withNext } from "../lib/continuation";
import { describeFailure, type Failure } from "../lib/error";
import { getSlackFirstUseLink, startSlackFirstUseLink, type SlackFirstUseLink } from "../lib/slack";
import { AuthCopy, AuthError, AuthFact, AuthFacts, AuthLink, AuthMissing, AuthSuccess, AuthTitle, AuthWindow, AuthWorking } from "../ui/AuthWindow";
import { IDText } from "../ui/IDText";
import { cx, ui } from "../ui/styles";

const MESSAGES = {
  forbidden: "This Helmr account cannot access the organization connected to this agent. Ask an administrator for an invitation, or sign in with the account that already has access.",
  bad_request: "This linking request has expired or is unavailable. Try your action in Slack again.",
  conflict: "This Slack account or your Helmr account is already linked differently in this workspace. Unlink the other account in My account, then try your action in Slack again.",
  service_unavailable: "Slack is not configured on this server. Ask an administrator to configure the Slack integration.",
};

export function SlackConnect() {
  const location = useLocation();
  const token = () => new URLSearchParams(location.search).get("link") ?? "";
  const [context, setContext] = createSignal<SlackFirstUseLink>();
  const [signIn, setSignIn] = createSignal(false);
  const [error, setError] = createSignal<Failure>();
  const [busy, setBusy] = createSignal(false);
  let mounted = true;
  onCleanup(() => { mounted = false; });
  onMount(async () => {
    if (!token()) return;
    try {
      const result = await getSlackFirstUseLink(token());
      if (mounted) setContext(result);
    } catch (e) {
      if (!mounted) return;
      if (e instanceof ApiError && e.status === 401) setSignIn(true);
      else setError(describeFailure(e, MESSAGES, "This linking request is unavailable. Try your action in Slack again."));
    }
  });
  const start = async () => {
    setBusy(true); setError(undefined);
    try {
      const result = await startSlackFirstUseLink(token());
      if (mounted) window.location.assign(result.redirect_url);
    } catch (e) {
      if (mounted) { setError(describeFailure(e, MESSAGES, "Slack linking could not start. Try again, or try your action in Slack again.")); setBusy(false); }
    }
  };
  const switchAccount = async () => {
    setBusy(true);
    try {
      await postJson("/api/auth/logout", {});
      window.location.assign(withNext("/login", locationDestination(location)));
    } catch (e) {
      if (mounted) { setError(describeFailure(e, {}, "Sign out failed. Try again.")); setBusy(false); }
    }
  };
  const switchButton = () => <AuthLink disabled={busy()} onClick={() => void switchAccount()}>Use a different Helmr account</AuthLink>;
  return <Switch fallback={<AuthWorking>Checking your linking request…</AuthWorking>}>
    <Match when={!token()}>
      <AuthWindow>
        <AuthMissing title="This Slack link is incomplete" action={<A class={cx(ui.button, "auth-full")} href="/">Go to the console</A>}>
          The link is missing its linking request. Try your action in Slack again and open the new link it sends.
        </AuthMissing>
      </AuthWindow>
    </Match>
    <Match when={signIn()}>
      <AuthWindow>
        <AuthTitle>Link your Slack account</AuthTitle>
        <AuthCopy>Sign in to Helmr to connect the Slack account you used to request work. Your action has not been applied.</AuthCopy>
        <A class={cx(ui.button, "auth-full")} href={withNext("/login", locationDestination(location))}>Sign in to Helmr</A>
      </AuthWindow>
    </Match>
    <Match when={context()?.linked ? context() : undefined}>
      {(current) => (
        <AuthWindow>
          <AuthSuccess title="Your accounts are already linked" action={<>
            <a class={cx(ui.button, "auth-full")} href={current().return_url} rel="noreferrer">Return to Slack</a>
            {switchButton()}
          </>}>
            Return to Slack and send a new message or repeat your action.
          </AuthSuccess>
        </AuthWindow>
      )}
    </Match>
    <Match when={context() || error()}>
      <AuthWindow>
        <AuthTitle>Link your Slack account</AuthTitle>
        <Show when={context()}>
          {(current) => <>
            <AuthFacts label="Accounts">
              <AuthFact label="Slack">{current().workspace_name || <IDText value={current().team_id} />}</AuthFact>
              <AuthFact label="Helmr">{current().helmr_display_name} in {current().organization_name}</AuthFact>
            </AuthFacts>
            <AuthCopy>Next, verify the Slack account that requested work. You will confirm both accounts before they are linked.</AuthCopy>
          </>}
        </Show>
        <Show when={error()}>{(failure) => <AuthError message={failure().message} details={failure().details} />}</Show>
        <Show when={context()} fallback={<A class={cx(ui.button, "auth-full")} href="/settings/account">My account</A>}>
          <button type="button" class={ui.button} disabled={busy()} onClick={() => void start()}>{busy() ? "Opening Slack…" : "Verify my Slack account"}</button>
        </Show>
        {switchButton()}
      </AuthWindow>
    </Match>
  </Switch>;
}
