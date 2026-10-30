import { useQueryClient } from "@tanstack/solid-query";
import { createResource, createSignal, onCleanup, onMount, Show, type JSX } from "solid-js";
import { getAccount, getAuthConfig, getMe, startGitHubLogin, type Me } from "../lib/auth";
import { assertPasskey } from "../lib/passkey";
import { AuthDivider } from "../ui/AuthWindow";
import { Dialog } from "../ui/Dialog";
import { Notice } from "../ui/Notice";
import { cx, ui } from "../ui/styles";
import { EmailSignIn } from "./EmailSignIn";

type Request = { operator: boolean; reason?: string };

// Keep the original page and any form dialog mounted throughout authentication.
// The interrupted action is never replayed: the person reviews and submits it again.
export function AuthenticationNotice() {
  const [request, setRequest] = createSignal<Request>();
  const show = (event: Event) => {
    if (!request()) setRequest((event as CustomEvent<Request>).detail);
  };
  onMount(() => window.addEventListener("helmr-confirmation-required", show));
  onCleanup(() => window.removeEventListener("helmr-confirmation-required", show));
  const close = () => setRequest(undefined);
  return <Show when={request()} keyed>
    {value => value.operator
      ? <OperatorConfirmation reason={value.reason} onClose={close} />
      : <AccountConfirmation reason={value.reason} onClose={close} />}
  </Show>;
}

function AccountConfirmation(props: { onClose: () => void; reason?: string | undefined }) {
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

  const methods = () => config()?.methods ?? [];
  const hasButtons = () => methods().includes("passkey") || methods().includes("github");
  return (
    <ConfirmationDialog
      intro="Confirm it’s you to continue. Your form stays open; nothing is submitted for you."
      reason={props.reason}
      error={error() || (config.error ? "Sign-in methods could not be loaded. Close and try again." : "")}
      settled={!config.loading}
      busy={busy()}
      complete={complete()}
      onClose={props.onClose}
    >
      <Show when={!verified()} fallback={<div><button type="button" class={ui.button} disabled={busy()} onClick={() => void run(confirmed)}>Retry confirmation</button></div>}>
        <Show when={hasButtons()}>
          <div class={ui.buttonGroup}>
            <Show when={methods().includes("passkey")}><button type="button" class={ui.button} disabled={busy()} onClick={() => void run(async () => { await assertPasskey("/api/auth/passkey/options", "/api/auth/passkey/verify"); await confirmed(); })}>Confirm with a passkey</button></Show>
            <Show when={methods().includes("github")}><button type="button" class={ui.secondaryButton} disabled={busy()} onClick={github}>Confirm with GitHub</button></Show>
          </div>
        </Show>
        <Show when={methods().includes("email")}>
          <div>
            <Show when={hasButtons()}><AuthDivider>or by email</AuthDivider></Show>
            <EmailSignIn compact {...(email() ? { email: email()! } : {})} complete={() => void run(confirmed)} />
          </div>
        </Show>
      </Show>
    </ConfirmationDialog>
  );
}

function OperatorConfirmation(props: { onClose: () => void; reason?: string | undefined }) {
  const queries = useQueryClient();
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal("");
  const [complete, setComplete] = createSignal(false);
  const confirm = async () => {
    setBusy(true); setError("");
    try {
      await assertPasskey("/admin/api/v1/auth/options", "/admin/api/v1/auth/verify");
      await queries.invalidateQueries({ queryKey: ["operator"] });
      setComplete(true);
    } catch (cause) { setError(cause instanceof Error ? cause.message : "The operator key could not be confirmed."); }
    finally { setBusy(false); }
  };
  return (
    <ConfirmationDialog
      intro="Confirm with an operator key to continue. Your form stays open; nothing is submitted for you."
      reason={props.reason}
      error={error()}
      settled
      busy={busy()}
      complete={complete()}
      onClose={props.onClose}
    >
      <div><button type="button" class={ui.button} disabled={busy()} onClick={() => void confirm()}>{busy() ? "Waiting for key…" : "Use operator key"}</button></div>
    </ConfirmationDialog>
  );
}

// The shared frame: why confirmation is needed, the methods, then either Cancel
// or, once confirmed, the way back to the retained action.
function ConfirmationDialog(props: {
  intro: string;
  reason?: string | undefined;
  error: string;
  settled: boolean;
  busy: boolean;
  complete: boolean;
  onClose: () => void;
  children: JSX.Element;
}) {
  let body: HTMLDivElement | undefined;
  let returnButton: HTMLButtonElement | undefined;
  // The dialog's own initial focus runs before the methods load; move it to the
  // first method once they render, and to the way back once confirmed.
  const focusFirst = () => requestAnimationFrame(() => body?.querySelector<HTMLElement>("button:not(:disabled), input:not(:disabled)")?.focus());
  return (
    <Dialog title="Confirm your sign-in" onClose={props.onClose} closeDisabled={props.busy} bodyClass="flex flex-col">
      <Show when={!props.complete} fallback={<>
        <div class="p-3"><Notice tone="success">Sign-in confirmed. Review your input, then submit your change.</Notice></div>
        <div class={ui.dialogFooter}><button ref={el => { returnButton = el; requestAnimationFrame(() => returnButton?.focus()); }} type="button" class={ui.button} onClick={props.onClose}>Return to your action</button></div>
      </>}>
        <div class="min-h-0 overflow-y-auto p-3">
          <div class={ui.blockStack}>
            <p class="m-0 text-body">{props.intro}</p>
            <Show when={props.reason}>
              {reason => (
                <dl class="m-0 rounded-xs bg-console-surface-muted px-2.5 py-2">
                  <dt class={ui.microLabel}>Needed for</dt>
                  <dd class="m-0 mt-0.5 text-body">{reason()}</dd>
                </dl>
              )}
            </Show>
            <Show when={props.error}><Notice tone="danger">{props.error}</Notice></Show>
            <Show when={props.settled} fallback={<p class={cx(ui.muted, "m-0")}>Loading your sign-in methods…</p>}>
              <div class={ui.blockStack} ref={el => { body = el; focusFirst(); }}>{props.children}</div>
            </Show>
          </div>
        </div>
        <div class={ui.dialogFooter}><button type="button" class={ui.secondaryButton} disabled={props.busy} onClick={props.onClose}>Cancel</button></div>
      </Show>
    </Dialog>
  );
}
