import { A } from "@solidjs/router";
import { createResource,Show } from "solid-js";
import { request } from "../lib/api";
import { getAccount } from "../lib/auth";
import { OrganizationNew } from "./organization-new";
import { AuthTitle,AuthCopy,AuthWindow } from "../ui/AuthWindow";
import { ui } from "../ui/styles";
export function Setup(){
 const [operator]=createResource(()=>request<{owner_setup_required:boolean}>("/api/operator/setup",{redirectOnUnauthorized:false}));
 const [account]=createResource(getAccount);
 return <Show when={!operator.loading&&!account.loading}>
  <Show when={operator()} fallback={<AuthWindow><AuthTitle>Set up Helmr</AuthTitle><AuthCopy>First, sign in with the operator key issued by your deployment.</AuthCopy><A href="/operator/login" class={ui.button}>Operator sign-in</A></AuthWindow>}>
   <Show when={operator()?.owner_setup_required} fallback={<AuthWindow><AuthTitle>Setup is complete</AuthTitle><A class={ui.button} href="/admin">Open deployment administration</A><A class={ui.secondaryButton} href="/">Open the console</A></AuthWindow>}>
    <Show when={account()} fallback={<AuthWindow><AuthTitle>Choose the first owner</AuthTitle><AuthCopy>Sign in to the ordinary account that will own this organization. Your operator credential remains separate.</AuthCopy><A class={ui.button} href="/login?next=%2Fsetup">Sign in to the owner account</A></AuthWindow>}><OrganizationNew /></Show>
   </Show>
  </Show>
 </Show>;
}
