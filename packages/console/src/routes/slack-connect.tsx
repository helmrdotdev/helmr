import { A, useLocation } from "@solidjs/router";
import { createSignal, onCleanup, onMount, Show } from "solid-js";
import { ApiError, postJson } from "../lib/api";
import { locationDestination, withNext } from "../lib/continuation";
import { getSlackFirstUseLink, startSlackFirstUseLink, type SlackFirstUseLink } from "../lib/slack";
import { AuthCopy, AuthScreen, AuthTitle } from "../ui/AuthScreen";
import { ui } from "../ui/styles";

export function SlackConnect() {
  const location = useLocation();
  const token = () => new URLSearchParams(location.search).get("link") ?? "";
  const [context, setContext] = createSignal<SlackFirstUseLink>();
  const [signIn, setSignIn] = createSignal(false);
  const [error, setError] = createSignal<string>();
  const [busy, setBusy] = createSignal(false);
  let mounted = true;
  onCleanup(() => { mounted = false; });
  onMount(async () => {
    try {
      const result = await getSlackFirstUseLink(token());
      if (mounted) setContext(result);
    } catch (e) {
      if (!mounted) return;
      if (e instanceof ApiError && e.status === 401) setSignIn(true);
      else if (e instanceof ApiError && e.status === 403) setError("This Helmr account cannot access the organization connected to this Agent. Ask an administrator for an invitation, or sign in with the account that already has access.");
      else setError(e instanceof Error ? e.message : "This linking request is unavailable. Try your action in Slack again.");
    }
  });
  const start = async () => {
    setBusy(true); setError(undefined);
    try {
      const result = await startSlackFirstUseLink(token());
      if (mounted) window.location.assign(result.redirect_url);
    } catch (e) {
      if (mounted) { setError(e instanceof Error ? e.message : "Slack linking could not start."); setBusy(false); }
    }
  };
  const switchAccount = async () => {
    setBusy(true);
    try {
      await postJson("/api/auth/logout", {});
      window.location.assign(withNext("/login", locationDestination(location)));
    } catch (e) {
      if (mounted) { setError(e instanceof Error ? e.message : "Sign out failed."); setBusy(false); }
    }
  };
  return <AuthScreen>
    <AuthTitle>Link your Slack account</AuthTitle>
    <Show when={signIn()}>
      <AuthCopy>Sign in to Helmr to connect the Slack account you used to request work. Your action has not been applied.</AuthCopy>
      <A class={ui.button} href={withNext("/login", locationDestination(location))}>Sign in to Helmr</A>
    </Show>
    <Show when={context()}>{(current) => <>
      <AuthCopy>Slack workspace: <strong>{current().workspace_name || current().team_id}</strong></AuthCopy>
      <AuthCopy>Helmr account: <strong>{current().helmr_display_name}</strong> in <strong>{current().organization_name}</strong></AuthCopy>
      <Show when={!current().linked} fallback={<>
        <AuthCopy>Your accounts are already linked. Return to Slack and send a new message or repeat your action.</AuthCopy>
        <a class={ui.button} href={current().return_url} rel="noreferrer">Return to Slack</a>
      </>}>
        <AuthCopy>Next, verify the Slack account that requested work. You will confirm both accounts before they are linked.</AuthCopy>
        <button class={ui.button} disabled={busy()} onClick={() => void start()}>{busy() ? "Opening Slack..." : "Verify my Slack account"}</button>
      </Show>
    </>}</Show>
    <Show when={error()}><p role="alert" class={ui.error}>{error()}</p></Show>
    <Show when={!signIn() && (context() || error())}>
      <button class={ui.secondaryButton} disabled={busy()} onClick={() => void switchAccount()}>Use a different Helmr account</button>
    </Show>
    <Show when={!signIn() && !context() && !error()}><AuthCopy>Checking your linking request...</AuthCopy></Show>
  </AuthScreen>;
}
