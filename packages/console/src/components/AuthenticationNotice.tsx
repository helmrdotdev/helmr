import { createSignal, onCleanup, onMount, Show } from "solid-js";
import { Dialog } from "../ui/Dialog";
import { ui } from "../ui/styles";

// A separate modal keeps the original form mounted and gives confirmation its
// own focus scope, including when the form is already inside a dialog.
export function AuthenticationNotice() {
  const [destination, setDestination] = createSignal("");
  const show = (event: Event) => {
    const detail = (event as CustomEvent<{ operator: boolean; signIn?: boolean }>).detail;
    setDestination(detail.operator ? "/admin/login" : detail.signIn ? "/login" : "/settings/account");
  };
  onMount(() => window.addEventListener("helmr-confirmation-required", show));
  onCleanup(() => window.removeEventListener("helmr-confirmation-required", show));
  return <Show when={destination()}>
    <Dialog title="Confirm your sign-in" onClose={() => setDestination("")}>
      <p>Sign in with the same account in the new tab, then return here and retry your action. Your form stays open.</p>
      <a href={destination()} target="_blank" rel="noopener noreferrer" class={ui.button}>Confirm sign-in</a>
      <button type="button" class={ui.secondaryButton} onClick={() => setDestination("")}>Return to form</button>
    </Dialog>
  </Show>;
}
