import { useQueryClient } from "@tanstack/solid-query";
import { createSignal, Show } from "solid-js";
import { resendEmailChange, startEmailChange, verifyEmailChange } from "../../lib/account";
import type { EmailChallenge } from "../../lib/auth";
import { errorMessage } from "../../lib/error";
import { ME_QUERY_KEY } from "../../lib/me";
import { FormDialog } from "../../ui/Dialog";
import { Field, TextInput } from "../../ui/Field";
import { ui } from "../../ui/styles";

// Two steps in one dialog: send a code to the new address, then enter it. The
// login email changes only when the code is verified.
export function ChangeEmailDialog(props: { onClose: () => void; onChanged: () => void }) {
  const cache = useQueryClient();
  const [email, setEmail] = createSignal("");
  const [code, setCode] = createSignal("");
  const [challenge, setChallenge] = createSignal<EmailChallenge>();
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal("");
  const run = async (fn: () => Promise<void>) => {
    setBusy(true); setError("");
    try { await fn(); } catch (cause) { setError(errorMessage(cause)); } finally { setBusy(false); }
  };
  const submit = () => run(async () => {
    const current = challenge();
    if (!current) { setChallenge(await startEmailChange(email().trim())); return; }
    await verifyEmailChange(current.challenge_id, code().trim());
    await cache.invalidateQueries({ queryKey: ME_QUERY_KEY });
    props.onChanged();
    props.onClose();
  });
  return (
    <FormDialog
      title="Change login email"
      intro={challenge() ? <>Enter the code sent to <strong>{email()}</strong>.</> : "We send a verification code to the new address. Your login email changes once you enter it."}
      submitLabel={challenge() ? "Change email" : "Send code"}
      busyLabel={challenge() ? "Changing…" : "Sending…"}
      busy={busy()}
      disabled={challenge() ? !code().trim() : !email().trim()}
      error={error()}
      onSubmit={() => void submit()}
      onClose={props.onClose}
    >
      <Field label="New email">
        <TextInput type="email" autocomplete="email" value={email()} onInput={e => setEmail(e.currentTarget.value)} required disabled={!!challenge()} />
      </Field>
      <Show when={challenge()}>{current => <>
        <Field label="Verification code" hint={<>Expires {new Date(current().expires_at).toLocaleTimeString()}.</>}>
          <TextInput ref={el => requestAnimationFrame(() => el.focus())} value={code()} onInput={e => setCode(e.currentTarget.value)} inputmode="numeric" autocomplete="one-time-code" pattern="[0-9]{8}" maxlength={8} required />
        </Field>
        <div class={ui.buttonGroup}>
          <button type="button" class={ui.compactButton} disabled={busy()} onClick={() => void run(async () => { setChallenge(await resendEmailChange(current().challenge_id)); })}>Resend code</button>
          <button type="button" class={ui.compactButton} disabled={busy()} onClick={() => { setChallenge(undefined); setCode(""); setError(""); }}>Use another address</button>
        </div>
      </>}</Show>
    </FormDialog>
  );
}
