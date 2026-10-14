import { logout } from "../lib/auth";
import { AuthCopy, AuthWindow, AuthTitle } from "../ui/AuthWindow";
import { ui } from "../ui/styles";

export function AccessRequired() {
  return (
    <AuthWindow>
      <AuthTitle>Access required</AuthTitle>
      <AuthCopy>
        Ask an organization owner to invite this account before continuing.
      </AuthCopy>
      <button class={ui.button} type="button" onClick={() => void logout()}>
        Sign out
      </button>
    </AuthWindow>
  );
}
