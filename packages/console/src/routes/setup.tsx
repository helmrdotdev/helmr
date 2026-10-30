import { A, Navigate } from "@solidjs/router";
import { createQuery } from "@tanstack/solid-query";
import { createResource, Match, Switch } from "solid-js";
import { ApiError, request } from "../lib/api";
import { getAccount, getAuthConfig } from "../lib/auth";
import { withNext } from "../lib/continuation";
import { OrganizationNew } from "./organization-new";
import { AuthTitle, AuthCopy, AuthLink, AuthWindow, AuthError, AuthWorking } from "../ui/AuthWindow";
import { SETUP_PATH, setupStep } from "../lib/setup";
import { cx, ui } from "../ui/styles";

function needsSignIn(error: unknown): boolean {
  return error instanceof ApiError && (error.code === "authentication_required" || (error.status === 401 && error.code === "unauthorized"));
}

export function Setup() {
  // The step counter only shows while the deployment still needs its first owner.
  const [config] = createResource(getAuthConfig);
  const operator = createQuery(() => ({
    queryKey: ["operator", "setup"],
    queryFn: () => request<{ owner_setup_required: boolean }>("/admin/api/v1/setup", { redirectOnUnauthorized: false }),
    retry: false,
  }));
  const account = createQuery(() => ({
    queryKey: ["setup-account"],
    queryFn: getAccount,
    enabled: operator.isSuccess && operator.data.owner_setup_required,
    retry: false,
  }));

  return <Switch>
    <Match when={operator.isPending}><AuthWorking>Checking setup…</AuthWorking></Match>
    <Match when={needsSignIn(operator.error)}>
      <AuthWindow step={config()?.setup_required ? setupStep(1) : undefined}><AuthTitle>Sign in as the operator</AuthTitle>
        <AuthCopy>Use the operator key issued by your deployment. It is separate from your own account.</AuthCopy>
        <A href={withNext("/admin/login", SETUP_PATH)} class={cx(ui.button, "auth-full")}>Operator sign-in</A>
      </AuthWindow>
    </Match>
    <Match when={operator.isError}>
      <AuthWindow><AuthError title="Setup could not be loaded" message="Unable to check deployment setup. Try again." />
        <button class={ui.button} onClick={() => void operator.refetch()}>Retry</button>
      </AuthWindow>
    </Match>
    <Match when={operator.data && !operator.data.owner_setup_required}>
      <AuthWindow><AuthTitle>Setup is complete</AuthTitle>
        <AuthCopy>Helmr is ready. Invite your team from Members, or deploy your first agent.</AuthCopy>
        <A class={cx(ui.button, "auth-full")} href="/">Open the console</A>
        <AuthLink href="/admin">Open deployment administration</AuthLink>
      </AuthWindow>
    </Match>
    <Match when={account.isPending}><AuthWorking>Checking owner account…</AuthWorking></Match>
    {/* Step 2: the sign-in page itself explains it is for the first owner. */}
    <Match when={needsSignIn(account.error)}><Navigate href={withNext("/login", SETUP_PATH)} /></Match>
    <Match when={account.isError}>
      <AuthWindow><AuthError title="Owner account could not be loaded" message="Unable to check your account. Try again." />
        <button class={ui.button} onClick={() => void account.refetch()}>Retry</button>
      </AuthWindow>
    </Match>
    <Match when={account.data}><OrganizationNew setup /></Match>
  </Switch>;
}
