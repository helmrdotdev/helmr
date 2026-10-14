import { ME_QUERY_KEY, useMe } from "../lib/me";
import { locationDestination, withNext } from "../lib/continuation";
import { useNavigate, useLocation } from "@solidjs/router";
import { useQueryClient } from "@tanstack/solid-query";
import { createEffect, createMemo, createSignal, Show } from "solid-js";
import { createSignOut, onboardingSteps } from "../features/projects/onboarding";
import { errorDetails, errorMessage } from "../lib/error";
import { createOrganization } from "../lib/organizations";
import { AuthCopy, AuthError, AuthTitle, AuthWindow } from "../ui/AuthWindow";
import { createDerivedSlug, Field, FormActions, SecretInput, SlugInput, TextInput } from "../ui/Field";
import { Steps } from "../ui/Steps";
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
    <AuthWindow step="Step 1 of 2">
      <AuthTitle>Create your organization</AuthTitle>
      <div class={ui.blockStack}>
        <Steps label="Setup steps" steps={onboardingSteps("organization")} />
        <AuthCopy>Organizations own members, projects, environments, credentials, and sessions.</AuthCopy>
      </div>
      <form onSubmit={submit}>
        <Field label="Name">
          <TextInput value={name()} onInput={(event) => setName(event.currentTarget.value)} placeholder="Acme" autocomplete="organization" autofocus />
        </Field>
        <Field label="Slug">
          <SlugInput slug={slug} placeholder="acme" />
        </Field>
        <Show when={setupTokenRequired()}>
          <Field label="Setup token">
            <SecretInput value={setupToken()} onInput={(event) => setSetupToken(event.currentTarget.value)} placeholder="Setup token" />
          </Field>
        </Show>
        <Show when={error() ?? signOut.error()}>{(failure) => <AuthError message={failure().message} details={failure().details} />}</Show>
        <FormActions
          submitLabel="Create organization"
          busyLabel="Creating..."
          busy={submitting()}
          disabled={signOut.busy() || me.isPending || !name().trim() || !slug.value().trim() || (setupTokenRequired() && !setupToken().trim())}
          cancelLabel={signOut.label()}
          cancelDisabled={signOut.busy()}
          onCancel={() => void signOut.run()}
        />
      </form>
    </AuthWindow>
  );
}
