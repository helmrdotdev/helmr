import { useQueryClient } from "@tanstack/solid-query";
import { createResource, createSignal, onCleanup, onMount, Show } from "solid-js";
import { getAccount, getAuthConfig, getMe, startGitHubLogin, type Me } from "../lib/auth";
import { assertPasskey } from "../lib/passkey";
import { Dialog } from "../ui/Dialog";
import { ui } from "../ui/styles";
import { EmailSignIn } from "./EmailSignIn";

function AccountConfirmation(props: { onClose: () => void; removingCredential: boolean }) {
  const queries = useQueryClient();
  const knownAccount = queries.getQueryData<Me>(["me"]);
  const [config] = createResource(getAuthConfig);
  const [account] = createResource(getAccount);
  const email = () => (account.error ? undefined : account())?.primary_email ?? knownAccount?.primary_email;
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal("");
  const [complete, setComplete] = createSignal(false);
  const [verified, setVerified] = createSignal(false);
  let popup: Window | null = null;
  let active = true;

  async function confirmed() {
    setVerified(true);
    // Read through the identity-pinned client before returning to the retained form.
    // Neither authentication nor this refresh may replay its mutation.
    const current = await getMe();
    if (!active) return;
    queries.setQueryData(["me"], current);
    await queries.invalidateQueries({ queryKey: ["device-code"] });
    if (active) setComplete(true);
  }
  async function run(action: () => Promise<void>) {
    setBusy(true); setError("");
    try { await action(); }
    catch (cause) { if (active) setError(cause instanceof Error ? cause.message : "Sign-in could not be confirmed."); }
    finally { if (active) setBusy(false); }
  }
  function github() {
    popup = window.open("about:blank", "_blank", "popup,width=600,height=720");
    if (!popup) { setError("Allow the GitHub sign-in window, then try again."); return; }
    const target = popup;
    void run(async () => {
      try {
        const result = await startGitHubLogin("/auth/confirmation");
        if (active && !target.closed) target.location.href = result.redirect_url;
      } catch (cause) { target.close(); throw cause; }
    });
  }
  const returned = (event: MessageEvent) => {
    if (event.origin !== window.location.origin || !popup || event.source !== popup || event.data !== "helmr-sign-in-complete") return;
    popup.close(); popup = null;
    void run(confirmed);
  };
  onMount(() => window.addEventListener("message", returned));
  onCleanup(() => { active = false; window.removeEventListener("message", returned); popup?.close(); });

  return <Dialog title="Confirm your sign-in" onClose={props.onClose} closeDisabled={busy()} bodyClass="space-y-3 p-3">
    <Show when={complete()} fallback={<>
      <p class={ui.muted}>Confirm your identity to continue. Then review and submit your action.</p>
      <Show when={props.removingCredential}><p class={ui.muted}>Use a different sign-in method or passkey from the one you are removing.</p></Show>
      <Show when={error() || config.error}><p role="alert">{error() || "Sign-in methods could not be loaded. Close and try again."}</p></Show>
      <Show when={!verified()} fallback={<button type="button" class={ui.button} disabled={busy()} onClick={() => void run(confirmed)}>Retry confirmation</button>}>
      <div class="flex flex-wrap gap-2">
      <Show when={config()?.methods.includes("passkey")}><button type="button" class={ui.button} disabled={busy()} onClick={() => void run(async () => { await assertPasskey("/api/auth/passkey/options", "/api/auth/passkey/verify"); await confirmed(); })}>Confirm with a passkey</button></Show>
      <Show when={config()?.methods.includes("github")}><button type="button" class={ui.secondaryButton} disabled={busy()} onClick={github}>Confirm with GitHub</button></Show>
      </div>
      <Show when={config()?.methods.includes("email")}><EmailSignIn {...(email() ? {email:email()!} : {})} complete={() => void run(confirmed)} /></Show>
      </Show>
      <button type="button" class={ui.secondaryButton} disabled={busy()} onClick={props.onClose}>Cancel</button>
    </>}>
      <p role="status">Sign-in confirmed. Review your input, then submit your change.</p>
      <button type="button" class={ui.button} onClick={props.onClose}>Return to your action</button>
    </Show>
  </Dialog>;
}

// Keep the original page and any form dialog mounted throughout authentication.
export function AuthenticationNotice() {
  const [request, setRequest] = createSignal<{ operator: boolean; removingCredential?: boolean }>();
  const show = (event: Event) => {
    if (!request()) setRequest((event as CustomEvent<{ operator: boolean; removingCredential?: boolean }>).detail);
  };
  onMount(() => window.addEventListener("helmr-confirmation-required", show));
  onCleanup(() => window.removeEventListener("helmr-confirmation-required", show));
  return <Show when={request()} keyed>{value => <Show when={!value.operator} fallback={
    <Dialog title="Confirm your sign-in" onClose={() => setRequest(undefined)}>
      <p>Sign in with the same operator in the new tab, then return here and retry your action. Your form stays open.</p>
      <a href="/admin/login" target="_blank" rel="noopener noreferrer" class={ui.button}>Confirm sign-in</a>
      <button type="button" class={ui.secondaryButton} onClick={() => setRequest(undefined)}>Return to form</button>
    </Dialog>
  }><AccountConfirmation removingCredential={!!value.removingCredential} onClose={() => setRequest(undefined)} /></Show>}</Show>;
}
