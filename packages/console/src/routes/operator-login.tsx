import { createSignal, onMount, Show } from "solid-js";
import { postJson, request } from "../lib/api";
import { returnPath } from "../lib/continuation";
import { errorMessage } from "../lib/error";
import { assertPasskey, registerPasskey } from "../lib/passkey";
import { AuthCopy, AuthDivider, AuthTitle, AuthWindow } from "../ui/AuthWindow";
import { Field, SecretInput, TextInput } from "../ui/Field";
import { Notice } from "../ui/Notice";
import { Steps } from "../ui/Steps";
import { cx, ui } from "../ui/styles";

// An enrollment in progress survives a reload, so a registered key can be verified.
const ENROLLMENT_KEY = "helmr.operator-enrollment";
// The server keeps the name sent when the key is verified, so it is remembered too.
const LABEL_KEY = "helmr.operator-enrollment.label";

export function OperatorLogin() {
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal("");
  const signIn = async () => {
    setBusy(true); setError("");
    try {
      await assertPasskey("/admin/api/v1/auth/options", "/admin/api/v1/auth/verify");
      window.location.assign(returnPath(new URLSearchParams(window.location.search).get("next") ?? "/setup"));
    } catch (cause) { setError(errorMessage(cause)); setBusy(false); }
  };
  return (
    <AuthWindow>
      <AuthTitle>Operator sign-in</AuthTitle>
      <AuthCopy>Use your independent deployment key. This does not sign you in to an ordinary account.</AuthCopy>
      <div class={ui.blockStack}>
        <Show when={error()}><Notice tone="danger">{error()}</Notice></Show>
        <button type="button" class={ui.button} disabled={busy()} onClick={() => void signIn()}>{busy() ? "Waiting for key…" : "Sign in with operator key"}</button>
      </div>
      <AuthDivider>No key yet, or lost it?</AuthDivider>
      <SetUpKey />
    </AuthWindow>
  );
}

// Setting up or repairing a key: redeem the deployment-issued capability, register
// the key, then sign in with it once so a key that cannot sign in is never kept.
function SetUpKey() {
  const [enrollment, setEnrollmentID] = createSignal(sessionStorage.getItem(ENROLLMENT_KEY) ?? "");
  const [open, setOpen] = createSignal(!!enrollment());
  const [capability, setCapability] = createSignal("");
  const [label, setLabel] = createSignal(sessionStorage.getItem(LABEL_KEY) ?? "Operator key");
  const [registered, setRegistered] = createSignal(false);
  const [ready, setReady] = createSignal(false);
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal("");
  const setEnrollment = (id: string) => { setEnrollmentID(id); if (id) sessionStorage.setItem(ENROLLMENT_KEY, id); else { sessionStorage.removeItem(ENROLLMENT_KEY); sessionStorage.removeItem(LABEL_KEY); } };
  const run = async (fn: () => Promise<void>) => {
    setBusy(true); setError("");
    try { await fn(); } catch (cause) { setError(errorMessage(cause)); } finally { setBusy(false); }
  };
  onMount(() => {
    if (enrollment()) void run(async () => { setRegistered((await request<{ stage: string }>(`/admin/api/v1/enrollment/${enrollment()}`, { redirectOnUnauthorized: false })).stage === "assert"); });
  });
  const redeem = () => run(async () => {
    const result = await postJson<{ capability: string }, { enrollment_id: string }>("/admin/api/v1/enrollment/redeem", { capability: capability().trim() }, { redirectOnUnauthorized: false });
    setCapability(""); setEnrollment(result.enrollment_id);
  });
  const register = () => run(async () => {
    await registerPasskey(`/admin/api/v1/enrollment/${enrollment()}/options`, `/admin/api/v1/enrollment/${enrollment()}/complete`, label().trim());
    sessionStorage.setItem(LABEL_KEY, label().trim());
    setRegistered(true);
  });
  const verify = () => run(async () => {
    await assertPasskey(`/admin/api/v1/enrollment/${enrollment()}/assert/options`, `/admin/api/v1/enrollment/${enrollment()}/assert/verify`, { label: label().trim() });
    setEnrollment(""); setReady(true);
  });
  const startOver = () => { setEnrollment(""); setRegistered(false); setReady(false); setError(""); };
  const step = () => ready() ? 3 : registered() ? 2 : enrollment() ? 1 : 0;
  const state = (index: number) => step() > index ? "done" as const : step() === index ? "current" as const : "upcoming" as const;
  return (
    <Show when={open()} fallback={<button type="button" class={cx(ui.secondaryButton, "w-full")} onClick={() => setOpen(true)}>Set up or repair a key</button>}>
      <section aria-labelledby="operator-set-up" class={ui.blockStack}>
        <div class="flex items-center justify-between gap-2">
          <h2 id="operator-set-up" class="m-0 text-body font-medium">Set up or repair a key</h2>
          <Show when={step() > 0}><button type="button" class={ui.compactButton} disabled={busy()} onClick={startOver}>Start over</button></Show>
        </div>
        <div class={ui.blockStack}>
          <Steps label="Set up or repair a key" steps={[
            {
              title: "Enter the capability",
              state: state(0),
              description: "Issued by your deployment for setting up or repairing a key.",
              content: <form class={ui.blockStack} onSubmit={e => { e.preventDefault(); void redeem(); }}>
                <Field label="Setup or repair capability" flush><SecretInput value={capability()} onInput={e => setCapability(e.currentTarget.value)} autocomplete="off" disabled={busy()} /></Field>
                <div><button type="submit" class={ui.button} disabled={busy() || !capability().trim()}>Continue</button></div>
              </form>,
            },
            {
              title: "Register the key",
              state: state(1),
              description: "Your browser asks you to create the key on this device or a security key.",
              content: <form class="flex flex-wrap items-end gap-2" onSubmit={e => { e.preventDefault(); void register(); }}>
                <Field label="Key name" flush class="w-52 max-w-full"><TextInput value={label()} onInput={e => setLabel(e.currentTarget.value)} required maxlength={200} disabled={busy()} /></Field>
                <button type="submit" class={ui.button} disabled={busy() || !label().trim()}>{busy() ? "Waiting for key…" : "Register key"}</button>
              </form>,
            },
            {
              title: "Sign in with the new key",
              state: state(2),
              description: <span>Proves the key works before it is kept as <span class="font-mono text-console-text">{label()}</span>.</span>,
              content: <button type="button" class={ui.button} disabled={busy()} onClick={() => void verify()}>{busy() ? "Waiting for key…" : "Verify key"}</button>,
            },
          ]} />
          <Show when={ready()}><Notice tone="success">Your key is ready. Sign in with it above.</Notice></Show>
          <Show when={error()}><Notice tone="danger">{error()}</Notice></Show>
        </div>
      </section>
    </Show>
  );
}
