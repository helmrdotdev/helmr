import { A,useSearchParams } from "@solidjs/router";
import { createSignal,onMount,Show } from "solid-js";
import { getAccount,getAuthConfig,logout,type Account,type AuthConfig } from "../lib/auth";
import { postJson,chooseOrganization } from "../lib/api";
import { assertPasskey } from "../lib/passkey";
import { EmailSignIn } from "../components/EmailSignIn";
import { AuthCopy,AuthError,AuthWindow,AuthTitle } from "../ui/AuthWindow";
import { ui } from "../ui/styles";
type Summary={organization_id:string;organization_name:string;email:string;expires_at:string};
export function Invite(){
 const [params]=useSearchParams();const [token,setToken]=createSignal("");const [summary,setSummary]=createSignal<Summary>();
 const [account,setAccount]=createSignal<Account>();const [config,setConfig]=createSignal<AuthConfig>();const [error,setError]=createSignal("");const [busy,setBusy]=createSignal(false);
 async function refresh(){try{setAccount(await getAccount());}catch{setAccount(undefined);}}
 async function run(fn:()=>Promise<void>){setBusy(true);setError("");try{await fn();}catch(e){setError(e instanceof Error?e.message:"Unable to continue.");}finally{setBusy(false);}}
 onMount(()=>{const raw=params["token"];const value=Array.isArray(raw)?raw[0]:raw;history.replaceState({},"","/invite");setToken(value??"");void run(async()=>{if(!value)throw new Error("Open the full invitation link from your email.");setSummary(await postJson("/api/auth/invitations/resolve",{token:value},{redirectOnUnauthorized:false}));setConfig(await getAuthConfig());await refresh();});});
 return <AuthWindow><AuthTitle>Join an organization</AuthTitle><Show when={error()}><AuthError message={error()} /></Show><Show when={summary()}>{s=><>
  <AuthCopy>You are invited to <strong>{s().organization_name}</strong> as <strong>{s().email}</strong>. Accepting gives this account membership in the organization.</AuthCopy>
  <Show when={account()} fallback={<>
   <Show when={config()?.methods.includes("email")}><EmailSignIn email={s().email} invitationToken={token()} complete={()=>void refresh()} /></Show>
   <Show when={config()?.methods.includes("passkey")}><button class={ui.secondaryButton} disabled={busy()} onClick={()=>void run(async()=>{await assertPasskey("/api/auth/passkey/options","/api/auth/passkey/verify");await refresh();})}>Sign in with a passkey</button></Show>
   <AuthCopy><A href="/login">Other sign-in methods</A>: sign in, then reopen your invitation link to accept.</AuthCopy>
  </>}>{a=><>
   <AuthCopy>Signed in as {a().primary_email}.</AuthCopy>
   <button class={ui.button} disabled={busy()||a().primary_email.toLowerCase()!==s().email.toLowerCase()} onClick={()=>void run(async()=>{const accepted=await postJson<{token:string},{organization_id:string}>("/api/auth/invitations/accept",{token:token()},{redirectOnUnauthorized:false});chooseOrganization(accepted.organization_id);})}>Accept invitation</button>
   <button class={ui.secondaryButton} disabled={busy()} onClick={()=>void run(async()=>{await logout();setAccount(undefined);})}>Sign out to use another account</button>
  </>}</Show>
 </>}</Show></AuthWindow>;
}
