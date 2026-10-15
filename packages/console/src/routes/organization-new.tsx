import { ME_QUERY_KEY, useMe } from "../lib/me";
import { locationDestination, withNext } from "../lib/continuation";
import { useNavigate, useLocation } from "@solidjs/router";
import { useQueryClient } from "@tanstack/solid-query";
import { createEffect, createMemo, createSignal, Show } from "solid-js";
import { createSignOut } from "../features/projects/onboarding";
import { errorDetails, errorMessage } from "../lib/error";
import { createOrganization } from "../lib/organizations";
import { AuthAccount, AuthCopy, AuthError, AuthTitle, AuthWindow } from "../ui/AuthWindow";
import { createDerivedSlug, Field, SecretInput, SlugInput, TextInput } from "../ui/Field";
import { ui } from "../ui/styles";

export function OrganizationNew() {
  const navigate = useNavigate();
  const location = useLocation();
  const queryClient = useQueryClient();
  const [name, setName] = createSignal("");
  const slug = createDerivedSlug(name);
  const [setupToken, setSetupToken] = createSignal("");
  const [submitting, setSubmitting] = createSignal(false);
  const [error, setError] = createSignal<{ message: string; details: string | undefined } | null>(null);
  const me = useMe();
  const signOut = createSignOut();
  const setupTokenRequired = createMemo(() => !!me.data?.setup_token_required);

  createEffect(() => {
    if (me.data?.access_required) navigate(withNext("/access-required", locationDestination(location)), { replace: true });
  });

  async function submit(event: SubmitEvent) {
    event.preventDefault();
    setSubmitting(true);
    setError(null);
    try {
      const token = setupToken().trim();
      await createOrganization({
        name: name().trim(),
        slug: slug.value().trim(),
        ...(token ? { setup_token: token } : {}),
      });
      await queryClient.invalidateQueries({ queryKey: ME_QUERY_KEY });
      await queryClient.invalidateQueries({ queryKey: ["projects"] });
      navigate(locationDestination(location), { replace: true });
    } catch (e) {
      setError({ message: errorMessage(e, "The organization could not be created. Try again."), details: errorDetails(e) });
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <AuthWindow step="Step 1 of 2" account={<AuthAccount name={me.data?.display_name} signOutLabel={signOut.label()} signingOut={signOut.busy()} onSignOut={() => void signOut.run()} />}>
      <AuthTitle>Create your organization</AuthTitle>
      <AuthCopy>Organizations own members, projects, environments, credentials, and sessions.</AuthCopy>
      <form onSubmit={submit}>
        <Field label="Name">
          <TextInput value={name()} onInput={(event) => setName(event.currentTarget.value)} placeholder="Acme" autocomplete="organization" autofocus />
        </Field>
        <Field label="Organization ID" hint="Lowercase letters, numbers and hyphens. Used in URLs and the CLI; it cannot be changed later.">
          <SlugInput slug={slug} placeholder="acme" />
        </Field>
        <Show when={setupTokenRequired()}>
          <Field label="Setup token">
            <SecretInput value={setupToken()} onInput={(event) => setSetupToken(event.currentTarget.value)} placeholder="Setup token" />
          </Field>
        </Show>
        <Show when={signOut.error()}>{(failure) => <AuthError message={failure().message} details={failure().details} />}</Show>
        <Show when={error()}>{(failure) => <AuthError message={failure().message} details={failure().details} />}</Show>
        <button type="submit" class={ui.button} disabled={submitting() || signOut.busy() || me.isPending || !name().trim() || !slug.value().trim() || (setupTokenRequired() && !setupToken().trim())}>
          {submitting() ? "Creating..." : "Create organization"}
        </button>
      </form>
    </AuthWindow>
  );
}
