import { createSignal,Show } from "solid-js";
import { returnPath } from "../lib/continuation";
import { postJson } from "../lib/api";
import { assertPasskey,registerPasskey } from "../lib/passkey";
import { AuthTitle,AuthCopy,AuthWindow,AuthError } from "../ui/AuthWindow";
import { Field,SecretInput,TextInput } from "../ui/Field";
import { ui } from "../ui/styles";

export function OperatorLogin(){
 const [capability,setCapability]=createSignal("");const [label,setLabel]=createSignal("Operator key");
 const [enrollment,setEnrollment]=createSignal("");const [staged,setStaged]=createSignal(false);const [ready,setReady]=createSignal(false);
 const [busy,setBusy]=createSignal(false);const [error,setError]=createSignal("");
 async function run(fn:()=>Promise<void>){setBusy(true);setError("");try{await fn();}catch(e){setError(e instanceof Error?e.message:"Operator authentication failed.");}finally{setBusy(false);}}
 return <AuthWindow><AuthTitle>Operator sign-in</AuthTitle><AuthCopy>Use your independent deployment key. This does not sign you in to an ordinary account.</AuthCopy>
 <Show when={error()}><AuthError message={error()} /></Show>
 <button class={ui.button} disabled={busy()} onClick={()=>void run(async()=>{await assertPasskey("/admin/api/v1/auth/options","/admin/api/v1/auth/verify");window.location.assign(returnPath(new URLSearchParams(window.location.search).get("next") ?? "/setup"));})}>Sign in with operator key</button>
 <details class="mt-6"><summary>Set up or repair an operator key</summary>
 <Show when={!enrollment()} fallback={<>
  <Field label="Key name"><TextInput value={label()} onInput={e=>setLabel(e.currentTarget.value)} disabled={busy()||staged()} /></Field>
  <Show when={!staged()} fallback={<Show when={!ready()} fallback={<p role="status">Your key is ready. Sign in with it above.</p>}><p>Now verify that you can sign in with the key you just registered.</p><button class={ui.button} disabled={busy()} onClick={()=>void run(async()=>{await assertPasskey(`/admin/api/v1/enrollment/${enrollment()}/assert/options`,`/admin/api/v1/enrollment/${enrollment()}/assert/verify`,{label:label()});setReady(true);})}>Verify this key</button></Show>}>
   <button class={ui.button} disabled={busy()} onClick={()=>void run(async()=>{await registerPasskey(`/admin/api/v1/enrollment/${enrollment()}/options`,`/admin/api/v1/enrollment/${enrollment()}/complete`,label());setStaged(true);})}>Register a key</button>
  </Show>
 </>}><form onSubmit={e=>{e.preventDefault();void run(async()=>{const result=await postJson<{capability:string},{enrollment_id:string}>("/admin/api/v1/enrollment/redeem",{capability:capability().trim()},{redirectOnUnauthorized:false});setCapability("");setEnrollment(result.enrollment_id);});}}>
 <Field label="Setup or repair capability"><SecretInput value={capability()} onInput={e=>setCapability(e.currentTarget.value)} autocomplete="off" /></Field><button class={ui.secondaryButton} disabled={busy()||!capability()}>Continue</button></form></Show>
 </details></AuthWindow>;
}
