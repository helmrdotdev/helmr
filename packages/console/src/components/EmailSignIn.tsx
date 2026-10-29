import { createEffect, createSignal, onCleanup, Show } from "solid-js";
import { startEmailLogin,resendEmailCode,verifyEmailCode,type EmailChallenge } from "../lib/auth";
import { AuthCopy,AuthError } from "../ui/AuthWindow";
import { Field,TextInput } from "../ui/Field";
import { loadError, AUTH_MESSAGES } from "../lib/error";
import { ui } from "../ui/styles";

export function EmailSignIn(props:{email?:string;invitationToken?:string;next?:string;complete:(next:string)=>void}){
 const [email,setEmail]=createSignal(props.email??"");const [code,setCode]=createSignal("");
 const [challenge,setChallenge]=createSignal<EmailChallenge>();const [busy,setBusy]=createSignal(false);const [error,setError]=createSignal("");
 let generation=0;
 createEffect(()=>{const boundEmail=props.email;generation++;setBusy(false);if(boundEmail!==undefined){setEmail(boundEmail);setChallenge(undefined);setCode("");setError("");}});
 const [now,setNow]=createSignal(Date.now());const timer=setInterval(()=>setNow(Date.now()),1000);onCleanup(()=>{generation++;clearInterval(timer);});
 const cooldown=()=>Math.max(0,Math.ceil((Date.parse(challenge()?.resend_after??"")-now())/1000)||0);
 const expired=()=>!!challenge()&&Date.parse(challenge()!.expires_at)<=now();
 async function run(fn:(current:()=>boolean)=>Promise<void>){const version=generation;const current=()=>generation===version;setBusy(true);setError("");try{await fn(current);}catch(e){if(current())setError(loadError(e,"Sign-in failed. Try again.",{...AUTH_MESSAGES,bad_request:challenge()?"Check the verification code and try again.":"Enter a valid email address.",invalid_token:"This code is invalid or expired. Try again or request a new code."}));}finally{if(current())setBusy(false);}}
 function start(){return run(async current=>{const result=await startEmailLogin({email:email().trim(),...(props.next?{next:props.next}:{}),...(props.invitationToken?{invitation_token:props.invitationToken}:{})});if(current()){setChallenge(result);setCode("");}});}
 return <form onSubmit={e=>{e.preventDefault();if(challenge())void run(async current=>{const result=await verifyEmailCode(challenge()!.challenge_id,code().trim());if(current())props.complete(result.redirect_after);});else void start();}}>
  <Show when={!challenge()} fallback={<>
   <AuthCopy>Enter the code sent to <strong>{email()}</strong>. You can read the email on another device.</AuthCopy>
   <Field label="Verification code"><TextInput value={code()} onInput={e=>setCode(e.currentTarget.value)} inputmode="numeric" autocomplete="one-time-code" pattern="[0-9]{8}" maxlength={8} required disabled={busy()||expired()} /></Field>
   <Show when={expired()}><p role="status">This code has expired. Request a new code.</p></Show>
  </>}><Field label="Email"><TextInput type="email" autocomplete="email" value={email()} onInput={e=>setEmail(e.currentTarget.value)} required disabled={busy()||!!props.email} /></Field></Show>
  <Show when={error()}><AuthError message={error()} /></Show>
  <button class={ui.button} type="submit" disabled={busy()||expired()}>{busy()?"Working…":challenge()?"Verify email":"Send verification code"}</button>
  <Show when={challenge()}><div class="mt-3 flex gap-3">
   <button class={ui.secondaryButton} type="button" disabled={busy()||cooldown()>0} onClick={()=>expired()?void start():void run(async current=>{const result=await resendEmailCode(challenge()!.challenge_id);if(current()){setChallenge(result);setCode("");}})}>{cooldown()>0?`Resend in ${cooldown()}s`:expired()?"Request new code":"Resend code"}</button>
   <button class={ui.secondaryButton} type="button" disabled={busy()} onClick={()=>{setChallenge(undefined);setCode("");}}>Start again</button>
  </div></Show>
 </form>;
}
