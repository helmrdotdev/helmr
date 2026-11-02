import { useSearchParams } from "@solidjs/router";
import { createResource,createSignal,Show } from "solid-js";
import { getAuthConfig,startGitHubLogin } from "../lib/auth";
import { AUTH_MESSAGES,loadError } from "../lib/error";
import { startSSO } from "../lib/sso";
import { Field,TextInput } from "../ui/Field";
import { returnPath } from "../lib/continuation";
import { SETUP_PATH, setupStep } from "../lib/setup";
import { assertPasskey } from "../lib/passkey";
import { EmailSignIn } from "../components/EmailSignIn";
import { AuthCopy,AuthDivider,AuthError,AuthWindow,AuthTitle } from "../ui/AuthWindow";
import { ui } from "../ui/styles";

export function Login(){
 const [params]=useSearchParams();const [config]=createResource(getAuthConfig);
 const [busy,setBusy]=createSignal(false);const [error,setError]=createSignal("");
 const next=()=>returnPath(typeof params["next"]==="string"?params["next"]:undefined);
 async function run(fn:()=>Promise<void>){setBusy(true);setError("");try{await fn();}catch(e){setError(loadError(e,"Sign-in failed.",AUTH_MESSAGES));setBusy(false);}}
 // A SAML sign-in the server refused comes back here with its code.
 const ssoError=()=>{const code=params["sso_error"];return typeof code==="string"?AUTH_MESSAGES[code]??AUTH_MESSAGES["authentication_invalid"]!:"";};
 const [choosingSSO,setChoosingSSO]=createSignal(false);const [organization,setOrganizationSlug]=createSignal("");
 const cloud=()=>config()?.deployment_mode==="managed-cloud";
 const sso=()=>run(async()=>{const result=await startSSO({next:next(),...(cloud()?{organization:organization().trim()}:{})});window.location.assign(result.redirect_url);});
 // From first-run setup this sign-in chooses the first owner, so it says so.
 const setup=()=>next()===SETUP_PATH&&config()?.setup_required===true;
 const hasPasskey=()=>config()?.methods.includes("passkey")??false;
 return <AuthWindow step={setup()?setupStep(2):undefined}><AuthTitle>{setup()?"Sign in as the first owner":"Sign in to Helmr"}</AuthTitle><AuthCopy>{setup()?"Use the account that will own this organization. Your operator key stays separate.":"Use your account to open your organizations."}</AuthCopy>
  <Show when={error()||ssoError()||config.error}><AuthError message={error()||ssoError()||"Sign-in methods could not be loaded. Reload to try again."} /></Show>
  <Show when={config()?.methods.includes("passkey")}><button class={ui.button} disabled={busy()} onClick={()=>void run(async()=>{await assertPasskey("/api/auth/passkey/options","/api/auth/passkey/verify");window.location.assign(next());})}>Continue with a passkey</button></Show>
  <Show when={config()?.methods.includes("github")}><button class={ui.secondaryButton} disabled={busy()} onClick={()=>void run(async()=>{const result=await startGitHubLogin(next());window.location.assign(result.redirect_url);})}>Continue with GitHub</button></Show>
  <Show when={config()?.methods.includes("sso")}><Show when={cloud()&&choosingSSO()} fallback={<button class={ui.secondaryButton} disabled={busy()} onClick={()=>cloud()?setChoosingSSO(true):void sso()}>Continue with company SSO</button>}>
   <form onSubmit={e=>{e.preventDefault();void sso();}}><Field label="Organization" hint="The organization name in your Helmr address."><TextInput value={organization()} onInput={e=>setOrganizationSlug(e.currentTarget.value)} autocomplete="organization" required disabled={busy()} /></Field><button class={ui.secondaryButton} type="submit" disabled={busy()||!organization().trim()}>Continue with company SSO</button></form>
  </Show></Show>
  <Show when={config()?.methods.includes("email")}><Show when={hasPasskey()||config()?.methods.includes("github")||config()?.methods.includes("sso")}><AuthDivider>or by email</AuthDivider></Show><EmailSignIn compact={hasPasskey()} next={next()} complete={destination=>window.location.assign(returnPath(destination))} /></Show>
 </AuthWindow>;
}
