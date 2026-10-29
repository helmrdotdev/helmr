import { useQueryClient } from "@tanstack/solid-query";
import { createResource,createSignal,For,Show } from "solid-js";
import { del,postJson,request } from "../../lib/api";
import { getAccount,getAuthConfig,type EmailChallenge } from "../../lib/auth";
import { assertPasskey,registerPasskey } from "../../lib/passkey";
import { EmailSignIn } from "../../components/EmailSignIn";
import { Panel } from "../../ui/Panel";
import { Field,TextInput } from "../../ui/Field";
import { ui } from "../../ui/styles";
type Credential={id:string;method:string;label:string;created_at:string;last_used_at?:string};
export function Credentials(){
 const queryClient=useQueryClient();
 async function confirmed(){await queryClient.invalidateQueries({queryKey:["me"]});setNotice("Account confirmed. Retry your change.");}
 const [account,{refetch:reloadAccount}]=createResource(getAccount);const [config]=createResource(getAuthConfig);
 const [credentials,{refetch}]=createResource(()=>request<{credentials:Credential[]}>("/api/account/credentials"));
 const [busy,setBusy]=createSignal(false);const [error,setError]=createSignal("");const [notice,setNotice]=createSignal("");
 const [label,setLabel]=createSignal("");const [name,setName]=createSignal<string>();const [email,setEmail]=createSignal("");const [code,setCode]=createSignal("");const [challenge,setChallenge]=createSignal<EmailChallenge>();const [remove,setRemove]=createSignal<Credential>();const [revoke,setRevoke]=createSignal(false);
 async function run(fn:()=>Promise<void>){setBusy(true);setError("");setNotice("");try{await fn();await refetch();}catch(e){setError(e instanceof Error?e.message:"Unable to update account.");}finally{setBusy(false);}}
 return <>
 <Show when={error()}><p role="alert" class={ui.muted}>{error()}</p></Show><Show when={notice()}><p role="status">{notice()}</p></Show>
 <Panel title="Your profile"><form onSubmit={e=>{e.preventDefault();void run(async()=>{await request("/api/account",{method:"PATCH",body:JSON.stringify({display_name:name()??account()?.display_name??""})});await reloadAccount();setNotice("Profile saved.");});}}><p>{account()?.display_name} · {account()?.primary_email}</p><Field label="Display name"><TextInput value={name()??account()?.display_name??""} onInput={e=>setName(e.currentTarget.value)} required maxlength={200}/></Field><button class={ui.button} disabled={busy()}>Save name</button></form></Panel>
 <Panel title="Confirm your account"><p class={ui.muted}>Sensitive changes need a sign-in within the last hour. To remove a sign-in method, confirm with a different method you will keep.</p>
 <Show when={config()?.methods.includes("passkey")}><button class={ui.button} disabled={busy()} onClick={()=>void run(async()=>{await assertPasskey("/api/auth/passkey/options","/api/auth/passkey/verify");await confirmed();})}>Confirm with a passkey</button></Show>
 <Show when={config()?.methods.includes("email")&&account()?.primary_email}><EmailSignIn email={account()!.primary_email} complete={()=>{void confirmed();void refetch();}}/></Show>
 <Show when={config()?.methods.includes("github")}><button class={ui.secondaryButton} disabled={busy()} onClick={()=>void run(async()=>{const result=await postJson<{next:string},{redirect_url:string}>("/api/auth/github/start",{next:"/settings/account"});window.location.assign(result.redirect_url);})}>Confirm with GitHub</button></Show>
 </Panel>
 <Panel title="Sign-in methods"><For each={credentials()?.credentials}>{c=><div class="flex items-center justify-between gap-3 py-2"><div>{c.label} <span class={ui.muted}>({c.method})</span></div><button class={ui.compactButton} disabled={busy()} onClick={()=>setRemove(c)}>Remove</button></div>}</For>
 <Show when={remove()}>{c=><div role="alert"><p>Remove {c().label}? Access authenticated through this method, including CLI and Slack access, will stop. Other confirmed methods remain available.</p><button class={ui.button} disabled={busy()} onClick={()=>void run(async()=>{await del(`/api/account/credentials/${c().id}`);setRemove(undefined);setNotice("Sign-in method removed.");})}>Remove method</button><button class={ui.secondaryButton} onClick={()=>setRemove(undefined)}>Cancel</button></div>}</Show>
 <form onSubmit={e=>{e.preventDefault();void run(async()=>{await registerPasskey("/api/account/passkeys/options","/api/account/passkeys/complete",label());setLabel("");setNotice("Passkey added.");});}}><Field label="Passkey name"><TextInput value={label()} onInput={e=>setLabel(e.currentTarget.value)} required maxlength={200}/></Field><button class={ui.button} disabled={busy()}>Add passkey</button></form>
 <Show when={config()?.methods.includes("github")}><button class={ui.secondaryButton} disabled={busy()} onClick={()=>void run(async()=>{const result=await postJson<{}, {redirect_url:string}>("/api/account/providers/github/link",{});window.location.assign(result.redirect_url);})}>Link GitHub to this account</button></Show>
 </Panel>
 <Show when={config()?.email_delivery}><Panel title="Change login email"><form onSubmit={e=>{e.preventDefault();void run(async()=>{if(!challenge()){setChallenge(await postJson<{new_email:string},EmailChallenge>("/api/account/email-change/start",{new_email:email()}));return;}await postJson("/api/account/email-change/verify",{challenge_id:challenge()!.challenge_id,code:code()});setChallenge(undefined);setCode("");await reloadAccount();setNotice("Login email changed. Access authenticated through the previous email has been revoked.");});}}>
 <Field label="New email"><TextInput type="email" value={email()} onInput={e=>setEmail(e.currentTarget.value)} required disabled={!!challenge()}/></Field>
 <Show when={challenge()}><Field label="Verification code"><TextInput value={code()} onInput={e=>setCode(e.currentTarget.value)} inputmode="numeric" autocomplete="one-time-code" pattern="[0-9]{8}" required/></Field><p>Expires {new Date(challenge()!.expires_at).toLocaleTimeString()}.</p></Show>
 <button class={ui.button} disabled={busy()}>{challenge()?"Verify and change email":"Verify new email"}</button>
 <Show when={challenge()}><button type="button" class={ui.secondaryButton} disabled={busy()} onClick={()=>void run(async()=>{setChallenge(await postJson("/api/account/email-change/resend",{challenge_id:challenge()!.challenge_id}));})}>Resend code</button><button type="button" class={ui.secondaryButton} onClick={()=>{setChallenge(undefined);setCode("");}}>Start again</button></Show>
 </form></Panel></Show>
 <Panel title="Revoke account access"><p>Sign out every browser and CLI and revoke Slack access issued for this account. Your sign-in methods and organization memberships remain.</p><Show when={revoke()} fallback={<button class={ui.secondaryButton} onClick={()=>setRevoke(true)}>Revoke all access…</button>}><button class={ui.button} disabled={busy()} onClick={()=>void run(async()=>{await postJson("/api/account/access/revoke-all",{});window.location.assign("/login");})}>Confirm revoke all access</button><button class={ui.secondaryButton} onClick={()=>setRevoke(false)}>Cancel</button></Show></Panel>
 </>;
}
