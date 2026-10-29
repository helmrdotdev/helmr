import { A } from "@solidjs/router";
import { createQuery } from "@tanstack/solid-query";
import { Show,type JSX } from "solid-js";
import { request } from "../lib/api";
import { AuthMissing,AuthWindow } from "../ui/AuthWindow";
import { ui } from "../ui/styles";

export function RequireAdmin(props:{children:JSX.Element}){
 const operator=createQuery(()=>({queryKey:["operator"],queryFn:()=>request<{id:string;expires_at:string}>("/api/operator/me",{redirectOnUnauthorized:false}),retry:false}));
 return <Show when={!operator.isPending}><Show when={operator.data} fallback={<AuthWindow><AuthMissing title="Operator sign-in required" action={<A href="/operator/login" class={ui.button}>Sign in as an operator</A>}>Deployment operations use an independent operator credential.</AuthMissing></AuthWindow>}>{props.children}</Show></Show>;
}
