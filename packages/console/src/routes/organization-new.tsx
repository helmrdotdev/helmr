import { useNavigate } from "@solidjs/router";
import { useQueryClient } from "@tanstack/solid-query";
import { createSignal, Show } from "solid-js";
import { ApiError } from "../lib/api";
import { createOrganization } from "../lib/organizations";
import { AuthCopy, AuthScreen, AuthTitle } from "../ui/AuthScreen";
import { ui } from "../ui/styles";

function slugify(value: string): string {
  return value
    .toLowerCase()
    .trim()
    .replace(/[^a-z0-9\s-]/g, "")
    .replace(/\s+/g, "-")
    .replace(/-+/g, "-")
    .replace(/^-|-$/g, "")
    .slice(0, 48)
    .replace(/^-|-$/g, "");
}

function createErrorMessage(error: unknown): string {
  if (error instanceof ApiError) return error.message;
  return "Something went wrong.";
}

export function OrganizationNew() {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [name, setName] = createSignal("");
  const [slug, setSlug] = createSignal("");
  const [slugTouched, setSlugTouched] = createSignal(false);
  const [submitting, setSubmitting] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);

  async function submit(event: SubmitEvent) {
    event.preventDefault();
    const nextName = name().trim();
    const nextSlug = slug().trim();
    if (!nextName || !nextSlug) {
      setError("Name and slug are required.");
      return;
    }
    setSubmitting(true);
    setError(null);
    try {
      await createOrganization({ name: nextName, slug: nextSlug });
      await queryClient.invalidateQueries({ queryKey: ["me"] });
      await queryClient.invalidateQueries({ queryKey: ["projects"] });
      navigate("/projects/new", { replace: true });
    } catch (e) {
      setError(createErrorMessage(e));
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <AuthScreen>
      <AuthTitle>Create your organization</AuthTitle>
      <AuthCopy>
        Organizations own members, projects, environments, credentials, and runs.
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
        <Show when={error()}>
          <p class={ui.fieldError} role="alert">{error()}</p>
        </Show>
        <button class={ui.button} type="submit" disabled={submitting() || !name().trim() || !slug().trim()}>
          {submitting() ? "Creating..." : "Create organization"}
        </button>
      </form>
    </AuthScreen>
  );
}
