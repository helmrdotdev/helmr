import { A } from "@solidjs/router";
import { createResource,For,Show } from "solid-js";
import { getOrganizations,getAuthConfig,logout } from "../lib/auth";
import { returnPath, withNext } from "../lib/continuation";
import { chooseOrganization } from "../lib/api";
import { AuthTitle,AuthWindow,AuthCopy,AuthError } from "../ui/AuthWindow";
import { ui } from "../ui/styles";
export function Organizations(){
 const next=returnPath(new URLSearchParams(window.location.search).get("next"));
 const [memberships]=createResource(getOrganizations);const [config]=createResource(getAuthConfig);
 return <AuthWindow><AuthTitle>Your organizations</AuthTitle><Show when={memberships.error}><AuthError message="Your organizations could not be loaded. Sign in again to continue." /></Show>
 <For each={memberships()?.organizations}>{org=><button class={ui.button} onClick={()=>chooseOrganization(org.id,next)}>{org.name}</button>}</For>
 <Show when={memberships()?.organizations.length===0}><AuthCopy>You do not belong to an organization yet. Open an invitation from your administrator.</AuthCopy></Show>
 <Show when={config()?.organization_creation_enabled}><A class={ui.secondaryButton} href={withNext("/organizations/new",next)}>Create an organization</A></Show>
 <A href="/settings/account" class={ui.secondaryButton}>Account settings</A><button class={ui.secondaryButton} onClick={()=>void logout()}>Sign out</button>
 </AuthWindow>;
}
