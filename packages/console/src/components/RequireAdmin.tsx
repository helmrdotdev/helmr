import { A, useLocation } from "@solidjs/router";
import { createQuery } from "@tanstack/solid-query";
import { createEffect,Show,type JSX } from "solid-js";
import { withNext, locationDestination } from "../lib/continuation";
import { ApiError,request } from "../lib/api";
import { AuthMissing,AuthWindow } from "../ui/AuthWindow";
import { ui } from "../ui/styles";

export function RequireAdmin(props:{children:JSX.Element}){
 const location = useLocation();
 const operator=createQuery(()=>({queryKey:["operator"],queryFn:()=>request<{id:string;expires_at:string}>("/admin/api/v1/me",{redirectOnUnauthorized:false}),retry:false}));
 createEffect(()=>{if(operator.data && operator.error instanceof ApiError && (operator.error.status===401 || operator.error.code==="authentication_required"))window.dispatchEvent(new CustomEvent("helmr-confirmation-required",{detail:{operator:true,signIn:true}}));});
 return <Show when={!operator.isPending}><Show when={operator.data} fallback={<AuthWindow><AuthMissing title="Operator sign-in required" action={<A href={withNext("/admin/login", locationDestination(location))} class={ui.button}>Sign in as an operator</A>}>Deployment operations use an independent operator credential.</AuthMissing></AuthWindow>}>{props.children}</Show></Show>;
}
