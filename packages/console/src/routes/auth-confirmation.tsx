import { onMount } from "solid-js";
import { AuthSuccess, AuthWindow } from "../ui/AuthWindow";

// Only signals the opener to verify the current account; never approves an action.
export function AuthConfirmation() {
  onMount(() => window.opener?.postMessage("helmr-sign-in-complete", window.location.origin));
  return <AuthWindow><AuthSuccess title="Sign-in complete">Return to the window where you started to review your action.</AuthSuccess></AuthWindow>;
}
