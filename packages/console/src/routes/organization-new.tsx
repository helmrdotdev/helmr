import { slugify } from "../lib/slug";
import { ME_QUERY_KEY, useMe } from "../lib/me";
import { locationDestination, withNext } from "../lib/continuation";
import { useNavigate, useLocation } from "@solidjs/router";
import { useQueryClient } from "@tanstack/solid-query";
import { createEffect, createMemo, createSignal, Show } from "solid-js";
import { errorMessage } from "../lib/error";
import { createOrganization } from "../lib/organizations";
import { AuthCopy, AuthWindow, AuthTitle } from "../ui/AuthWindow";
import { ui } from "../ui/styles";


export function OrganizationNew() {
  const navigate = useNavigate();
  const location = useLocation();
  const queryClient = useQueryClient();
  const [name, setName] = createSignal("");
  const [slug, setSlug] = createSignal("");
  const [setupToken, setSetupToken] = createSignal("");
  const [slugTouched, setSlugTouched] = createSignal(false);
  const [submitting, setSubmitting] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);
  const me = useMe();
  const setupTokenRequired = createMemo(() => !!me.data?.setup_token_required);


  createEffect(() => {
    if (me.data?.access_required) navigate(withNext("/access-required", locationDestination(location)), { replace: true });
  });

  async function submit(event: SubmitEvent) {
    event.preventDefault();
    const nextName = name().trim();
    const nextSlug = slug().trim();
    const nextSetupToken = setupToken().trim();
    if (!nextName || !nextSlug || (setupTokenRequired() && !nextSetupToken)) {
      setError(setupTokenRequired() ? "Name, slug, and setup token are required." : "Name and slug are required.");
      return;
    }
    setSubmitting(true);
    setError(null);
    try {
      await createOrganization({
        name: nextName,
        slug: nextSlug,
        ...(nextSetupToken ? { setup_token: nextSetupToken } : {}),
      });
      await queryClient.invalidateQueries({ queryKey: ME_QUERY_KEY });
      await queryClient.invalidateQueries({ queryKey: ["projects"] });
      navigate(locationDestination(location), { replace: true });
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <AuthWindow>
      <AuthTitle>Create your organization</AuthTitle>
      <AuthCopy>
        Organizations own members, projects, environments, credentials, and Sessions.
      </AuthCopy>
      <form onSubmit={submit}>
        <label class={ui.field}>
          <span>Name</span>
          <input
            type="text"
            class={ui.input}
            value={name()}
            onInput={(event) => {
              setName(event.currentTarget.value);
              if (!slugTouched()) setSlug(slugify(event.currentTarget.value));
            }}
            placeholder="Acme"
            autocomplete="organization"
            autofocus
          />
        </label>
        <label class={ui.field}>
          <span>Slug</span>
          <input
            type="text"
            class={ui.input}
            value={slug()}
            onInput={(event) => {
              setSlugTouched(true);
              setSlug(event.currentTarget.value);
            }}
            placeholder="acme"
            autocomplete="off"
            spellcheck={false}
          />
        </label>
        <Show when={setupTokenRequired()}>
          <label class={ui.field}>
            <span>Setup token</span>
            <input
              type="password"
              class={ui.input}
              value={setupToken()}
              onInput={(event) => setSetupToken(event.currentTarget.value)}
              placeholder="Setup token"
              autocomplete="off"
            />
          </label>
        </Show>
        <Show when={error()}>
          <p class={ui.fieldError} role="alert">{error()}</p>
        </Show>
        <button
          class={ui.button}
          type="submit"
          disabled={me.isPending || submitting() || !name().trim() || !slug().trim() || (setupTokenRequired() && !setupToken().trim())}
        >
          {submitting() ? "Creating..." : "Create organization"}
        </button>
      </form>
    </AuthWindow>
  );
}
