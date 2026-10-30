import { createSignOut } from "../features/projects/onboarding";
import { useMe } from "../lib/me";
import { AuthAccount, AuthCopy, AuthError, AuthWindow, AuthTitle } from "../ui/AuthWindow";
import { ui } from "../ui/styles";
import { Show } from "solid-js";

export function AccessRequired() {
  const me = useMe();
  const signOut = createSignOut();
  return (
    <AuthWindow account={<AuthAccount name={me.data?.display_name} signOutLabel={signOut.label()} signingOut={signOut.busy()} onSignOut={() => void signOut.run()} />}>
      <AuthTitle>Access required</AuthTitle>
      <AuthCopy>Ask an organization owner to invite this account, then open the invite link from your email.</AuthCopy>
      <Show when={signOut.error()}>{(failure) => <AuthError message={failure().message} details={failure().details} />}</Show>
      {/* Once an invitation is accepted elsewhere, the console opens instead of this page. */}
      <button type="button" class={ui.secondaryButton} onClick={() => window.location.assign("/")}>Check again</button>
    </AuthWindow>
  );
}
