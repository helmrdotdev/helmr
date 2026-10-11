import { A, useNavigate } from "@solidjs/router";
import { createSignal, onCleanup, onMount, Show } from "solid-js";
import { confirmSlackUserLink, verifySlackUserLink, type SlackIdentityProof } from "../lib/slack";
import { AuthCopy, AuthScreen, AuthTitle } from "../ui/AuthScreen";
import { ui } from "../ui/styles";

export function AuthSlackLink() {
  const navigate = useNavigate();
  const [proof, setProof] = createSignal<SlackIdentityProof | null>(null);
  const [error, setError] = createSignal<string | null>(null), [busy, setBusy] = createSignal(false);
  const [complete, setComplete] = createSignal(false);
  let mounted = true; onCleanup(() => { mounted = false; });
  onMount(async () => {
    const fragment = new URLSearchParams(window.location.hash.slice(1));
    const input = { state: fragment.get("state") ?? "", code: fragment.get("code") ?? "", error: fragment.get("error") ?? "" };
    history.replaceState({}, "", "/auth/slack/link");
    try { const result = await verifySlackUserLink(input); if (mounted) setProof(result); }
    catch (e) { if (mounted) setError(e instanceof Error ? e.message : "Slack identity could not be verified."); }
  });
  const confirm = async () => {
    const current = proof(); if (!current) return;
    setBusy(true); setError(null);
    try { await confirmSlackUserLink(current.confirmation); if (mounted) { if (current.return_url) setComplete(true); else navigate("/account/slack", { replace: true }); } }
    catch (e) { if (mounted) setError(e instanceof Error ? e.message : "Slack identity could not be linked."); }
    finally { if (mounted) setBusy(false); }
  };
  return <AuthScreen>
    <AuthTitle>{complete() ? "Your accounts are linked" : "Link your Slack account"}</AuthTitle>
    <Show when={!complete()} fallback={<>
      <AuthCopy>Return to Slack and send a new message or repeat your action. Linking does not apply your earlier action.</AuthCopy>
      <a class={ui.button} href={proof()?.return_url} rel="noreferrer">Return to Slack</a>
    </>}><Show when={proof()} fallback={<Show when={!error()}><AuthCopy>Verifying your Slack account...</AuthCopy></Show>}>{(current) => <div class="flex flex-col gap-3">
      <p>Slack account: <strong>{current().identity.name || current().identity.slack_user_id}</strong></p>
      <p class={ui.muted}>{current().identity.slack_user_id} in {current().workspace_name || current().identity.team_id}</p>
      <p>Helmr account: <strong>{current().helmr_display_name}</strong> in {current().helmr_org_name}</p>
      <p>Confirm that both accounts are yours. Slack actions will use this Helmr account’s current permissions. Linking does not change organization membership.</p>
      <button class={ui.button} disabled={busy()} onClick={() => void confirm()}>{busy() ? "Linking..." : "Confirm these accounts are mine"}</button>
    </div>}</Show></Show>
    <Show when={error()}><p class={ui.error} role="alert">{error()}</p><AuthCopy>Use the Helmr account that started linking. If the flow expired, try your action in Slack again or start from your Slack account settings.</AuthCopy></Show>
    <Show when={!complete()}><A href="/account/slack">Your Slack account</A></Show>
  </AuthScreen>;
}
