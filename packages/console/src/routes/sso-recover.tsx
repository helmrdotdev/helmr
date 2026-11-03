import { A, useSearchParams } from "@solidjs/router";
import { createSignal, onMount, Show } from "solid-js";
import { chooseOrganization } from "../lib/api";
import { getOrganizations, type OrganizationMembership } from "../lib/auth";
import { errorMessage } from "../lib/error";
import { recoverWithCode } from "../lib/sso";
import { AuthCopy, AuthTitle, AuthWindow } from "../ui/AuthWindow";
import { ChoiceCards, Field, TextInput } from "../ui/Field";
import { Notice } from "../ui/Notice";
import { cx, ui } from "../ui/styles";

// An owner whose identity provider is unavailable signs in another way and
// enters one recovery code to reach the organization.
export function SSORecover() {
  const [params] = useSearchParams();
  const [organizations, setOrganizations] = createSignal<OrganizationMembership[]>([]);
  const [organization, setOrganization] = createSignal(typeof params["organization"] === "string" ? params["organization"] : "");
  const [code, setCode] = createSignal("");
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal("");
  onMount(() => void getOrganizations().then(result => {
    setOrganizations(result.organizations);
    if (!organization() && result.organizations.length === 1) setOrganization(result.organizations[0]!.id);
  }).catch(cause => setError(errorMessage(cause))));
  const submit = async () => {
    setBusy(true); setError("");
    try { await recoverWithCode(organization(), code().trim()); chooseOrganization(organization()); }
    catch (cause) { setError(errorMessage(cause, "The recovery code did not work. Check it and try again.")); setBusy(false); }
  };
  return (
    <AuthWindow>
      <AuthTitle>Use a recovery code</AuthTitle>
      <AuthCopy>For owners when the company identity provider is unavailable. Each code works once, and every owner is notified.</AuthCopy>
      <Show when={error()}><Notice tone="danger">{error()}</Notice></Show>
      <form class={ui.blockStack} onSubmit={e => { e.preventDefault(); void submit(); }}>
        <Show when={organizations().length > 1}>
          <ChoiceCards label="Organization" name="recover-organization" value={organization()} onChange={setOrganization}
            options={organizations().map(o => ({ value: o.id, label: o.name }))} />
        </Show>
        <Field label="Recovery code"><TextInput value={code()} onInput={e => setCode(e.currentTarget.value)} autocomplete="one-time-code" required disabled={busy()} /></Field>
        <button type="submit" class={cx(ui.button, "auth-full")} disabled={busy() || !organization() || !code().trim()}>{busy() ? "Checking…" : "Continue"}</button>
      </form>
      <AuthCopy><A href="/login">Back to sign-in</A></AuthCopy>
    </AuthWindow>
  );
}

