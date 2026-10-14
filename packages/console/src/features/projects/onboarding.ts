import { createSignal } from "solid-js";
import { logout } from "../../lib/auth";
import { describeFailure, type Failure } from "../../lib/error";
import type { Step } from "../../ui/Steps";

// The two onboarding steps a new account goes through before reaching the console.
export function onboardingSteps(current: "organization" | "project"): Step[] {
  return [
    { title: "Create your organization", state: current === "organization" ? "current" : "done" },
    { title: "Create your first project", state: current === "project" ? "current" : "upcoming" },
  ];
}

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
