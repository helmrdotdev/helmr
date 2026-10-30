import { A } from "@solidjs/router";
import { createResource, For, Show } from "solid-js";
import { createSignOut } from "../features/projects/onboarding";
import { chooseOrganization, selectedOrganization } from "../lib/api";
import { getAccount, getAuthConfig, getOrganizations } from "../lib/auth";
import { returnPath, withNext } from "../lib/continuation";
import { AuthAccount, AuthCopy, AuthError, AuthTitle, AuthWindow } from "../ui/AuthWindow";
import { LinesSkeleton } from "../ui/Skeleton";
import { cx, ui } from "../ui/styles";

// Choosing an organization happens after sign-in and, rarely, when switching from
// the profile menu; the current one is marked and the console is one link away.
export function Organizations() {
  const next = returnPath(new URLSearchParams(window.location.search).get("next"));
  const [memberships] = createResource(getOrganizations);
  const [config] = createResource(getAuthConfig);
  const [account] = createResource(getAccount);
  const signOut = createSignOut();
  const current = selectedOrganization();
  const currentName = () => memberships()?.organizations.find(org => org.id === current)?.name;
  const count = () => memberships()?.organizations.length ?? 0;
  return (
    <AuthWindow account={
      <div class="flex min-w-0 items-center gap-2 font-mono text-chrome">
        <Show when={currentName()}>{name => <><A href="/" class="shrink-0 text-console-accent hover:text-console-accent-hover">Back to {name()}</A><span class="text-console-muted" aria-hidden="true">·</span></>}</Show>
        <AuthAccount name={account()?.primary_email} signOutLabel={signOut.label()} signingOut={signOut.busy()} onSignOut={() => void signOut.run()} />
      </div>
    }>
      <AuthTitle>Choose an organization</AuthTitle>
      <Show when={memberships.error}><AuthError message="Your organizations could not be loaded. Sign in again to continue." /></Show>
      <Show when={signOut.error()}>{failure => <AuthError message={failure().message} details={failure().details} />}</Show>
      <Show when={memberships()} fallback={<Show when={!memberships.error}><LinesSkeleton label="Loading your organizations" /></Show>}>
        <Show when={count() > 0} fallback={<AuthCopy>You do not belong to an organization yet. Open an invitation from your administrator.</AuthCopy>}>
          <AuthCopy>
            {count() === 1 ? "You belong to one organization." : `You belong to ${count()} organizations.`}
            <Show when={currentName()}>{name => <> You are in {name()} now; switch any time from your profile menu.</>}</Show>
          </AuthCopy>
          <nav aria-label="Your organizations" class="overflow-hidden rounded-xs border border-console-border-strong bg-console-surface">
            <For each={memberships()!.organizations}>
              {org => (
                <button
                  type="button"
                  aria-label={org.name}
                  aria-current={org.id === current ? "true" : undefined}
                  class={cx(
                    "flex w-full cursor-pointer items-center gap-3 border-0 border-b border-console-border-soft px-3.5 py-3 text-left last:border-b-0 hover:bg-console-bg-panel",
                    org.id === current ? "bg-console-accent-soft" : "bg-transparent",
                  )}
                  onClick={() => chooseOrganization(org.id, next)}
                >
                  <span class="flex min-w-0 flex-1 flex-col">
                    <span class="truncate text-body font-medium text-console-text">{org.name}</span>
                    <span class="truncate font-mono text-micro text-console-muted">{org.slug}</span>
                  </span>
                  <Show when={org.id === current}><span class="font-mono text-micro text-console-accent">Current</span></Show>
                  <span class="text-console-subtle" aria-hidden="true">›</span>
                </button>
              )}
            </For>
          </nav>
        </Show>
        <div class="mt-4 flex flex-wrap items-center justify-between gap-2">
          <Show when={config()?.organization_creation_enabled} fallback={<span />}>
            <A class={ui.secondaryButton} href={withNext("/organizations/new", next)}>Create an organization</A>
          </Show>
          <A href="/settings/account" class={ui.monoLink}>Profile</A>
        </div>
      </Show>
    </AuthWindow>
  );
}
