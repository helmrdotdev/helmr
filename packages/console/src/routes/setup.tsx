import { A } from "@solidjs/router";
import { createQuery } from "@tanstack/solid-query";
import { Match, Switch } from "solid-js";
import { ApiError, request } from "../lib/api";
import { getAccount } from "../lib/auth";
import { withNext } from "../lib/continuation";
import { OrganizationNew } from "./organization-new";
import { AuthTitle, AuthCopy, AuthWindow, AuthError, AuthWorking } from "../ui/AuthWindow";
import { ui } from "../ui/styles";

function needsSignIn(error: unknown): boolean {
  return error instanceof ApiError && (error.code === "authentication_required" || (error.status === 401 && error.code === "unauthorized"));
}

export function Setup() {
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
      <AuthWindow><AuthTitle>Set up Helmr</AuthTitle>
        <AuthCopy>First, sign in with the operator key issued by your deployment.</AuthCopy>
        <A href={withNext("/admin/login", "/setup")} class={ui.button}>Operator sign-in</A>
      </AuthWindow>
    </Match>
    <Match when={operator.isError}>
      <AuthWindow><AuthError title="Setup could not be loaded" message="Unable to check deployment setup. Try again." />
        <button class={ui.button} onClick={() => void operator.refetch()}>Retry</button>
      </AuthWindow>
    </Match>
    <Match when={operator.data && !operator.data.owner_setup_required}>
      <AuthWindow><AuthTitle>Setup is complete</AuthTitle>
        <A class={ui.button} href="/admin">Open deployment administration</A>
        <A class={ui.secondaryButton} href="/">Open the console</A>
      </AuthWindow>
    </Match>
    <Match when={account.isPending}><AuthWorking>Checking owner account…</AuthWorking></Match>
    <Match when={needsSignIn(account.error)}>
      <AuthWindow><AuthTitle>Choose the first owner</AuthTitle>
        <AuthCopy>Sign in to the ordinary account that will own this organization. Your operator credential remains separate.</AuthCopy>
        <A class={ui.button} href={withNext("/login", "/setup")}>Sign in to the owner account</A>
      </AuthWindow>
    </Match>
    <Match when={account.isError}>
      <AuthWindow><AuthError title="Owner account could not be loaded" message="Unable to check your account. Try again." />
        <button class={ui.button} onClick={() => void account.refetch()}>Retry</button>
      </AuthWindow>
    </Match>
    <Match when={account.data}><OrganizationNew /></Match>
  </Switch>;
}
