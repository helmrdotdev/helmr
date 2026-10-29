import { useSearchParams } from "@solidjs/router";
import { createResource,createSignal,Show } from "solid-js";
import { getAuthConfig,startGitHubLogin } from "../lib/auth";
import { returnPath } from "../lib/continuation";
import { assertPasskey } from "../lib/passkey";
import { EmailSignIn } from "../components/EmailSignIn";
import { AuthCopy,AuthDivider,AuthError,AuthWindow,AuthTitle } from "../ui/AuthWindow";
import { ui } from "../ui/styles";

export function Login(){
 const [params]=useSearchParams();const [config]=createResource(getAuthConfig);
 const [busy,setBusy]=createSignal(false);const [error,setError]=createSignal("");
 const next=()=>returnPath(typeof params["next"]==="string"?params["next"]:undefined);
 async function run(fn:()=>Promise<void>){setBusy(true);setError("");try{await fn();}catch(e){setError(e instanceof Error?e.message:"Sign-in failed.");setBusy(false);}}
 return <AuthWindow><AuthTitle>Sign in to Helmr</AuthTitle><AuthCopy>Use your account to open your organizations.</AuthCopy>
  <Show when={error()||config.error}><AuthError message={error()||"Sign-in methods could not be loaded. Reload to try again."} /></Show>
  <Show when={config()?.methods.includes("passkey")}><button class={ui.button} disabled={busy()} onClick={()=>void run(async()=>{await assertPasskey("/api/auth/passkey/options","/api/auth/passkey/verify");window.location.assign(next());})}>Continue with a passkey</button></Show>
  <Show when={config()?.methods.includes("github")}><button class={ui.secondaryButton} disabled={busy()} onClick={()=>void run(async()=>{const result=await startGitHubLogin(next());window.location.assign(result.redirect_url);})}>Continue with GitHub</button></Show>
  <Show when={config()?.methods.includes("email")}><AuthDivider>OR</AuthDivider><EmailSignIn next={next()} complete={destination=>window.location.assign(returnPath(destination))} /></Show>
 </AuthWindow>;
}
