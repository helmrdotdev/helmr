import { useSearchParams } from "@solidjs/router";
import { createResource, createSignal, Show } from "solid-js";
import { postJson } from "../lib/api";
import { getAuthConfig } from "../lib/auth";
import { returnPath } from "../lib/continuation";
import { SETUP_PATH, setupStep } from "../lib/setup";
import { errorMessage } from "../lib/error";
import { assertPasskey, registerPasskey } from "../lib/passkey";
import { AuthCopy, AuthDivider, AuthTitle, AuthWindow } from "../ui/AuthWindow";
import { Field, SecretInput, TextInput } from "../ui/Field";
import { Notice } from "../ui/Notice";
import { Steps } from "../ui/Steps";
import { cx, ui } from "../ui/styles";

export function OperatorLogin() {
  const [params] = useSearchParams();
  const [config] = createResource(getAuthConfig);
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
    <AuthWindow step={params["next"] === SETUP_PATH && config()?.setup_required ? setupStep(1) : undefined}>
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

// A capability works once, in one browser. Reloading, or reusing it, needs a new one.
const CAPABILITY_MESSAGES = {
  invalid_token: "This setup or repair capability has already been used or has expired. Run control-plane operator enroll (or control-plane operator repair) again to get a new one.",
};

// Setting up or repairing a key: redeem the deployment-issued capability, then
// register the key. Registering installs it as the operator's only key and
// signs out every operator session.
function SetUpKey() {
  const [open, setOpen] = createSignal(false);
  const [enrollment, setEnrollment] = createSignal("");
  const [capability, setCapability] = createSignal("");
  const [label, setLabel] = createSignal("Operator key");
  const [ready, setReady] = createSignal(false);
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal("");
  const run = async (fn: () => Promise<void>) => {
    setBusy(true); setError("");
    try { await fn(); } catch (cause) { setError(errorMessage(cause, undefined, CAPABILITY_MESSAGES)); } finally { setBusy(false); }
  };
  const redeem = () => run(async () => {
    const result = await postJson<{ capability: string }, { enrollment_id: string }>("/admin/api/v1/enrollment/redeem", { capability: capability().trim() }, { redirectOnUnauthorized: false });
    setCapability(""); setEnrollment(result.enrollment_id);
  });
  const register = () => run(async () => {
    await registerPasskey(`/admin/api/v1/enrollment/${enrollment()}/options`, `/admin/api/v1/enrollment/${enrollment()}/complete`, label().trim());
    setEnrollment(""); setReady(true);
  });
  const startOver = () => { setEnrollment(""); setReady(false); setError(""); };
  const step = () => ready() ? 2 : enrollment() ? 1 : 0;
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
              description: "Issued by your deployment for setting up or repairing a key. It works once, in one browser.",
              content: <form class={ui.blockStack} onSubmit={e => { e.preventDefault(); void redeem(); }}>
                <Field label="Setup or repair capability" flush><SecretInput value={capability()} onInput={e => setCapability(e.currentTarget.value)} autocomplete="off" disabled={busy()} /></Field>
                <div><button type="submit" class={ui.button} disabled={busy() || !capability().trim()}>Continue</button></div>
              </form>,
            },
            {
              title: "Register the key",
              state: state(1),
              description: "Your browser asks you to create the key on this device or a security key. It replaces any earlier operator keys.",
              content: <form class="flex flex-wrap items-end gap-2" onSubmit={e => { e.preventDefault(); void register(); }}>
                <Field label="Key name" flush class="w-52 max-w-full"><TextInput value={label()} onInput={e => setLabel(e.currentTarget.value)} required maxlength={200} disabled={busy()} /></Field>
                <button type="submit" class={ui.button} disabled={busy() || !label().trim()}>{busy() ? "Waiting for key…" : "Register key"}</button>
              </form>,
            },
          ]} />
          <Show when={ready()}><Notice tone="success">Your key is ready. Sign in with it above. If it cannot sign in, run <span class="font-mono">control-plane operator repair</span> for a new capability.</Notice></Show>
          <Show when={error()}><Notice tone="danger">{error()}</Notice></Show>
        </div>
      </section>
    </Show>
  );
}
