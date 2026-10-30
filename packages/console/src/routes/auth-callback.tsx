import { A, useNavigate, useSearchParams } from "@solidjs/router";
import { createSignal, Match, onMount, Switch } from "solid-js";
import { finishGitHubAuth } from "../lib/auth";
import { returnPath } from "../lib/continuation";
import { AUTH_MESSAGES, describeFailure, type Failure } from "../lib/error";
import { AuthError, AuthMissing, AuthWindow, AuthWorking } from "../ui/AuthWindow";
import { cx, ui } from "../ui/styles";

type Params = Record<string, string | undefined>;
type Outcome = { kind: "working" } | { kind: "missing" } | ({ kind: "failed" } & Failure);

function readParams(params: Record<string, string | string[] | undefined>): Params {
  return Object.fromEntries(Object.entries(params).map(([key, value]) => [key, Array.isArray(value) ? value[0] : value]));
}

// Where to go after a failed sign-in: the console, or the terminal for the CLI.
function NextStep() {
  return (
    <>
      <p>If you started from the CLI, run helmr login again in your terminal.</p>
      <A class={cx(ui.button, "auth-full")} href="/login">Sign in to the console</A>
    </>
  );
}

// The return from a sign-in provider: finish the sign-in with the parameters it
// sent, drop them from the address bar, and continue where the person started.
// `finish` returns null when a required parameter is missing.
function SignInCallback(props: { path: string; finish: (params: Params) => Promise<{ redirect_after: string }> | null }) {
  const [params] = useSearchParams();
  const navigate = useNavigate();
  const [outcome, setOutcome] = createSignal<Outcome>({ kind: "working" });

  onMount(async () => {
    const values = readParams(params);
    history.replaceState({}, "", props.path);
    const pending = props.finish(values);
    if (!pending) {
      setOutcome({ kind: "missing" });
      return;
    }
    try {
      const result = await pending;
      navigate(returnPath(result.redirect_after), { replace: true });
    } catch (error) {
      setOutcome({ kind: "failed", ...describeFailure(error, AUTH_MESSAGES, "Sign in could not be completed.") });
    }
  });

  return (
    <Switch fallback={<AuthWorking>Signing you in…</AuthWorking>}>
      <Match when={outcome().kind === "missing"}>
        <AuthWindow>
          <AuthMissing title="This sign-in link is incomplete" action={<NextStep />}>
            The link is missing the part that identifies your sign-in. Request a new sign-in link and open it in this browser.
          </AuthMissing>
        </AuthWindow>
      </Match>
      <Match when={outcome().kind === "failed" ? outcome() as Extract<Outcome, { kind: "failed" }> : undefined}>
        {(failure) => (
          <AuthWindow>
            <AuthError title="Sign in failed" message={failure().message} details={failure().details} action={<NextStep />} />
          </AuthWindow>
        )}
      </Match>
    </Switch>
  );
}

export function AuthGitHubCallback() {
  return (
    <SignInCallback
      path="/auth/github/callback"
      finish={(params) => finishGitHubAuth({
        code: params["code"] ?? "",
        state: params["state"] ?? "",
        ...(params["error"] !== undefined ? { error: params["error"] } : {}),
        ...(params["error_description"] !== undefined ? { error_description: params["error_description"] } : {}),
      })}
    />
  );
}
