import { createSignal } from "solid-js";
import { logout } from "../../lib/auth";
import { describeFailure, type Failure } from "../../lib/error";

// Leaving onboarding by signing out: busy while the request runs, and the
// failure when it does not complete.
export function createSignOut() {
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal<Failure | null>(null);
  return {
    busy,
    error,
    label: () => (busy() ? "Signing out..." : "Sign out"),
    run: async () => {
      setBusy(true);
      setError(null);
      try {
        await logout();
      } catch (cause) {
        setError(describeFailure(cause, {}, "Sign out failed. Try again."));
        setBusy(false);
      }
    },
  };
}
