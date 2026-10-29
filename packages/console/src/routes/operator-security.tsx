import { createResource,createSignal,onMount,For,Show } from "solid-js";
import { del,postJson,request } from "../lib/api";
import { getAuthConfig } from "../lib/auth";
import { assertPasskey,registerPasskey } from "../lib/passkey";
import { PageHeader } from "../ui/PageHeader";
import { Panel } from "../ui/Panel";
import { Field,SecretInput,TextInput } from "../ui/Field";
import { ui } from "../ui/styles";
type Credential={id:string;label:string};
export function OperatorSecurity(){
 const [operator,{refetch:refreshOperator}]=createResource(()=>request<{id:string;expires_at:string}>("/admin/api/v1/me",{redirectOnUnauthorized:false}));
 const [keys,{refetch}]=createResource(()=>request<{credentials:Credential[]}>("/admin/api/v1/credentials",{redirectOnUnauthorized:false}));const [config]=createResource(getAuthConfig);
 const [busy,setBusy]=createSignal(false);const [error,setError]=createSignal("");const [notice,setNotice]=createSignal("");const [remove,setRemove]=createSignal<Credential>();
 const [label,setLabel]=createSignal("Operator key");const [enrollment,setEnrollmentID]=createSignal(sessionStorage.getItem("helmr.operator-add-key")??"");
 const setEnrollment=(id:string)=>{setEnrollmentID(id);if(id)sessionStorage.setItem("helmr.operator-add-key",id);else sessionStorage.removeItem("helmr.operator-add-key");};const [staged,setStaged]=createSignal(false);
 const [capability,setCapability]=createSignal("");const [repair,setRepairID]=createSignal(sessionStorage.getItem("helmr.account-repair")??"");
 const setRepair=(id:string)=>{setRepairID(id);if(id)sessionStorage.setItem("helmr.account-repair",id);else sessionStorage.removeItem("helmr.account-repair");};const [target,setTarget]=createSignal("");
 async function run(fn:()=>Promise<void>){setBusy(true);setError("");setNotice("");try{await fn();await refetch();await refreshOperator();}catch(e){setError(e instanceof Error?e.message:"Unable to continue.");}finally{setBusy(false);}}
 onMount(()=>{void run(async()=>{
  if(enrollment()){const state=await request<{stage:string}>(`/admin/api/v1/enrollment/${enrollment()}`);setStaged(state.stage==="assert");}
  if(repair()){const state=await request<{target:string}>(`/admin/api/v1/account-repairs/${repair()}`);setTarget(state.target);}
 });});
 return <div class={ui.sectionStack}><PageHeader title="Operator security"/><Show when={error()}><p role="alert">{error()}</p></Show><Show when={notice()}><p role="status">{notice()}</p></Show>
 <Panel title="Operator session"><p>{operator()?.id}</p><p>Expires {operator()?new Date(operator()!.expires_at).toLocaleString():"…"}. Confirmation does not extend this deadline.</p>
 <button class={ui.button} disabled={busy()} onClick={()=>void run(async()=>{await assertPasskey("/admin/api/v1/auth/options","/admin/api/v1/auth/verify");setNotice("Operator confirmed for sensitive actions.");})}>Confirm with operator key</button>
 <button class={ui.secondaryButton} disabled={busy()} onClick={()=>void run(async()=>{await assertPasskey("/admin/api/v1/auth/renew/options","/admin/api/v1/auth/renew/verify");setNotice("New 12-hour operator session started.");})}>Reauthenticate and renew session</button>
 <button class={ui.secondaryButton} disabled={busy()} onClick={()=>void run(async()=>{await postJson("/admin/api/v1/auth/logout",{});window.location.assign("/admin/login");})}>Sign out operator</button>
 </Panel>
 <Panel title="Operator keys"><For each={keys()?.credentials}>{key=><div class="flex gap-3 py-2"><span>{key.label}</span><button class={ui.compactButton} onClick={()=>setRemove(key)}>Remove…</button></div>}</For>
 <Show when={remove()}>{key=><div><p>Remove {key().label}? First confirm with another operator key that you will keep.</p><button class={ui.button} disabled={busy()} onClick={()=>void run(async()=>{await del(`/admin/api/v1/credentials/${key().id}`);setRemove(undefined);setNotice("Operator key removed.");})}>Confirm removal</button><button class={ui.secondaryButton} onClick={()=>setRemove(undefined)}>Cancel</button></div>}</Show>
 <Show when={enrollment()}><button class={ui.secondaryButton} disabled={busy()} onClick={()=>{setEnrollment("");setStaged(false);setError("");}}>Start another key enrollment</button></Show>
 <Field label="New key name"><TextInput value={label()} onInput={e=>setLabel(e.currentTarget.value)} disabled={busy()||staged()}/></Field>
 <Show when={!staged()} fallback={<button class={ui.button} disabled={busy()} onClick={()=>void run(async()=>{await assertPasskey(`/admin/api/v1/enrollment/${enrollment()}/assert/options`,`/admin/api/v1/enrollment/${enrollment()}/assert/verify`,{label:label()});setEnrollment("");setStaged(false);setNotice("Operator key added and tested.");})}>Verify the new key</button>}>
 <button class={ui.button} disabled={busy()||!label()} onClick={()=>void run(async()=>{let id=enrollment();if(!id){id=(await postJson<{}, {enrollment_id:string}>("/admin/api/v1/credentials/enroll",{})).enrollment_id;setEnrollment(id);}await registerPasskey(`/admin/api/v1/enrollment/${id}/options`,`/admin/api/v1/enrollment/${id}/complete`,label());setStaged(true);})}>Register an additional operator key</button>
 </Show></Panel>
 <Show when={config()?.deployment_mode==="self-hosted"}><Panel title="Supervised account repair"><p>Use a deployment-issued capability for the selected ordinary account. The account holder must use their own authenticator while you supervise. This restores access; existing sign-in methods remain for the holder to review. It grants no organization membership or operator rights.</p>
 <Show when={repair()} fallback={<form onSubmit={e=>{e.preventDefault();void run(async()=>{const result=await postJson<{capability:string},{enrollment_id:string;target:string}>("/admin/api/v1/account-repairs/redeem",{capability:capability().trim()});setCapability("");setRepair(result.enrollment_id);setTarget(result.target);});}}><Field label="Account repair capability"><SecretInput value={capability()} onInput={e=>setCapability(e.currentTarget.value)} autocomplete="off"/></Field><button class={ui.button} disabled={busy()||!capability()}>Review repair target</button></form>}>
 <button class={ui.secondaryButton} disabled={busy()} onClick={()=>{setRepair("");setTarget("");}}>Use another repair capability</button><p>Repair target: <strong>{target()}</strong></p>
 <button class={ui.button} disabled={busy()||!target()} onClick={()=>void run(async()=>{await registerPasskey(`/admin/api/v1/account-repairs/${repair()}/options`,`/admin/api/v1/account-repairs/${repair()}/complete`,"Restored account key");setRepair("");setNotice("Account access restored. Existing access is revoked. The account holder can now sign in normally with their key.");})}>Account holder: register your key and complete repair</button>
 </Show>
 </Panel></Show></div>;
}
