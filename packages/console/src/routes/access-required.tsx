import { logout } from "../lib/auth";
import { AuthCopy, AuthScreen, AuthTitle } from "../ui/AuthScreen";
import { ui } from "../ui/styles";

export function AccessRequired() {
  return (
    <AuthScreen>
      <AuthTitle>Access required</AuthTitle>
      <AuthCopy>
        Ask an organization owner to invite this account before continuing.
      </AuthCopy>
      <button class={ui.button} type="button" onClick={() => void logout()}>
        Sign out
      </button>
    </AuthScreen>
  );
}
