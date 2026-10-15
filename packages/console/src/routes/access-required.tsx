import { createSignOut } from "../features/projects/onboarding";
import { useMe } from "../lib/me";
import { AuthAccount, AuthCopy, AuthError, AuthWindow, AuthTitle } from "../ui/AuthWindow";
import { Show } from "solid-js";

export function AccessRequired() {
  const me = useMe();
  const signOut = createSignOut();
  return (
    <AuthWindow account={<AuthAccount name={me.data?.display_name} signOutLabel={signOut.label()} signingOut={signOut.busy()} onSignOut={() => void signOut.run()} />}>
      <AuthTitle>Access required</AuthTitle>
      <AuthCopy>Ask an organization owner to invite this account, then open the invite link from your email.</AuthCopy>
      <Show when={signOut.error()}>{(failure) => <AuthError message={failure().message} details={failure().details} />}</Show>
    </AuthWindow>
  );
}
