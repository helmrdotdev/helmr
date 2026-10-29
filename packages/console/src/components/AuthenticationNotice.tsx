import { createSignal, onCleanup, onMount, Show } from "solid-js";
import { ui } from "../ui/styles";

// Keep the pending form intact. Confirmation opens separately; the user returns
// and explicitly retries. No operation payload is stored or replayed here.
export function AuthenticationNotice() {
  const [destination,setDestination] = createSignal("");
  const show = (event: Event) => setDestination((event as CustomEvent<{operator:boolean}>).detail.operator ? "/admin/login" : "/settings/account");
  onMount(()=>window.addEventListener("helmr-confirmation-required",show));
  onCleanup(()=>window.removeEventListener("helmr-confirmation-required",show));
  return <Show when={destination()}><aside role="alert" class="fixed bottom-4 right-4 z-50 max-w-sm rounded border border-console-border bg-console-surface p-4 shadow-lg">
    <p>Confirm your sign-in, then return here and retry your action.</p>
    <a href={destination()} target="_blank" rel="noopener noreferrer" class={ui.button}>Confirm sign-in</a>
    <button class={ui.secondaryButton} onClick={()=>setDestination("")}>Dismiss</button>
  </aside></Show>;
}
