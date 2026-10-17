import assert from 'node:assert/strict';
import {calls,server} from './mock-http';
import {OpenCode} from '@opencode/sdk/effect';
import {SessionRestart} from '@opencode/core/session/execution/restart';
import {SessionExecution} from '@opencode/core/session/execution';
import {SessionStore} from '@opencode/core/session/store';
import {SessionInbox} from '@opencode/core/session/inbox';
import {SessionMessage} from '@opencode/core/session/message';
import {Session} from '@opencode/core/session';
import {Job} from '@opencode/core/job';
import {Effect,Layer,Context} from 'effect';
console.error('PHASE opencode imports-ready',new Date().toISOString());
const root='/workspace/native-sdk/isolated';
let sessions:any,store:any,inbox:any,jobs:any,execution:any,ids:any[]=[];
const capture=(node:any,tag:any,fn:any)=>node.replace(node.mapLayer((layer:any)=>Layer.tap(layer,(ctx:any)=>Effect.sync(()=>fn(Context.get(ctx,tag))))));
const taps=[capture(Session.node,Session.Service,(s:any)=>sessions=s),capture(SessionStore.node,SessionStore.Service,(s:any)=>store=s),capture(SessionInbox.node,SessionInbox.Service,(s:any)=>inbox=s),capture(Job.node,Job.Service,(s:any)=>jobs=s),capture(SessionExecution.node,SessionExecution.Service,(s:any)=>execution=s)];
const inert=SessionRestart.node.replace(SessionRestart.node.mapLayer(()=>Layer.succeed(SessionRestart.Service,{resumeSuspendedSessions:Effect.void})));
const config={model:'fixture/probe',providers:{fixture:{name:'Offline fixture',package:'@opencode/ai/providers/anthropic',settings:{baseURL:'http://127.0.0.1:18479/v1',apiKey:'offline-fixture'},models:{probe:{name:'Probe',limit:{context:100000,output:1000},capabilities:{tools:true,input:['text'],output:['text']}}}}}};
const opts={database:{path:root+'/joined-'+Date.now()+'.sqlite'},config:{directory:root+'/config',project:false,claude:false,content:JSON.stringify(config)},models:{fetch:false,snapshot:false},fs:{filewatcher:false,fff:false},plugins:[]};
let retired=0;let seededRunning:any[]=[];
function recovery(cancel:boolean,failAfter=0){let ok:any,bad:any;const ready=new Promise((r,j)=>{ok=r;bad=j});
 const replacement=SessionRestart.node.replace(SessionRestart.node.mapLayer(()=>Layer.unwrap(Effect.gen(function*(){
  const original=yield* Session.Service;
  const nativeLayer=SessionRestart.layer({maxAttempts:0}).pipe(Layer.provide(Layer.succeed(Session.Service,{...original,synthetic:(input:any)=>original.synthetic({...input,resume:false})})));
  return Layer.effect(SessionRestart.Service,Effect.gen(function*(){const native=yield* SessionRestart.Service;return {resumeSuspendedSessions:Effect.gen(function*(){yield* native.resumeSuspendedSessions;if(cancel){for(const id of (yield* store.list()).map((s:any)=>s.id)){for(const row of yield* original.inbox(id)){yield* original.cancelInbox({sessionID:id,inboxID:row.id});retired++;if(failAfter&&retired===failAfter)yield* Effect.fail('injected inbox failure')}}}}).pipe(Effect.onExit(exit=>Effect.sync(()=>exit._tag==='Success'?ok():bad(exit))))}})).pipe(Layer.provide(nativeLayer));
 }))));return {replacement,ready};}
try{
 await Effect.runPromise(Effect.scoped(Effect.gen(function*(){console.error('PHASE opencode create',new Date().toISOString());const sdk=yield* OpenCode.create(opts,{overrides:[inert,...taps]});const p=yield* sdk.sessions.create({location:{directory:root}});const c=yield* sdk.sessions.create({location:{directory:root},parentID:p.id});const idle=yield* sdk.sessions.create({location:{directory:root}});const idleChild=yield* sdk.sessions.create({location:{directory:root},parentID:idle.id});ids=[p.id,c.id,idle.id,idleChild.id];yield* store.claim(p.id);yield* store.claim(c.id);
 for(const id of [p.id,c.id])for(const delivery of ['queue','steer'])yield* inbox.admit({id:SessionMessage.ID.create(),sessionID:id,item:{type:'user',delivery,payload:{text:'retired old '+delivery}}});
 yield* inbox.admitCompaction({id:SessionMessage.ID.create(),sessionID:c.id,delivery:'queue'});
 for(const [parent,child,suffix] of [[p.id,c.id,'claimed'],[idle.id,idleChild.id,'idle']]){const id='bg-'+suffix;yield* jobs.start({id,type:'subagent',notificationID:SessionMessage.ID.create(),recovery:{kind:'subagent',parentSessionID:parent,childSessionID:child,agent:'probe',description:'offline background'},run:Effect.succeed('old completed result')});yield* jobs.wait({id});yield* jobs.background(id)}
 for(const kind of ['shell','subagent']){const id='running-'+kind;const recovery=kind==='shell'?{kind,sessionID:idle.id,shellID:'offline-shell',command:'offline-held-command'}:{kind,parentSessionID:idle.id,childSessionID:idleChild.id,agent:'probe',description:'offline held child'};yield* jobs.start({id,type:kind,notificationID:SessionMessage.ID.create(),recovery,run:Effect.never});yield* jobs.background(id)};seededRunning=(yield* jobs.pendingBackground).filter((x:any)=>x.status==='running').map((x:any)=>x.recovery.kind);assert.equal(seededRunning.length,2);assert.equal(calls.length,0);
 })));
 // Deliberate stop after native recovery and before inbox retirement.
 await Effect.runPromise(Effect.scoped(Effect.gen(function*(){const r=recovery(true,1);yield* OpenCode.create(opts,{overrides:[r.replacement,...taps]});const outcome=yield* Effect.exit(Effect.tryPromise(()=>r.ready));assert.equal(outcome._tag,'Failure');assert.equal(calls.length,0);assert.equal((yield* store.listSuspended()).length,0);assert.equal((yield* jobs.pendingBackground).length,0);assert.ok((yield* sessions.inbox(ids[0])).length>=1)})));
 // Reopening converges and retires durable user + synthetic work before admissions.
 await Effect.runPromise(Effect.scoped(Effect.gen(function*(){const r=recovery(true);console.error('PHASE opencode create',new Date().toISOString());const sdk=yield* OpenCode.create(opts,{overrides:[r.replacement,...taps]});yield* Effect.promise(()=>r.ready);assert.equal(calls.length,0);for(const id of ids)assert.equal((yield* sessions.inbox(id)).length,0);console.error('PHASE opencode prompt',new Date().toISOString());yield* sdk.sessions.prompt({sessionID:ids[0],text:'fresh explicit input'});yield* execution.awaitIdle(ids[0]);assert.ok(calls.length>0);{const messages=yield* store.messages({sessionID:ids[0],order:'desc'});assert.equal(messages.find((x:any)=>x.type==='idle')?.outcome,'succeeded');assert.ok(messages.find((x:any)=>x.type==='assistant')?.content.some((x:any)=>x.type==='text'&&x.text==='offline reply'));}assert.ok(!JSON.stringify(calls).includes('retired old'));const before=calls.length;yield* sessions.synthetic({sessionID:ids[0],text:'ordinary runtime synthetic input',description:'probe'});yield* execution.awaitIdle(ids[0]);assert.ok(calls.length>before);{const messages=yield* store.messages({sessionID:ids[0],order:'desc'});assert.equal(messages.find((x:any)=>x.type==='idle')?.outcome,'succeeded');assert.ok(messages.find((x:any)=>x.type==='assistant')?.content.some((x:any)=>x.type==='text'&&x.text==='offline reply'));}console.log(JSON.stringify({case:'OpenCode-composed-recovery-local-model',retired,seededRunning,zeroRecoveryModelCalls:true,cancellationFailureRejectsReadiness:true,nativeEnumerationCoversOwnerDatabase:true,partialRecoveryReopenConverges:true,freshTerminalSuccess:true,oldInputsExcluded:true,freshInputWorks:true,ordinarySyntheticStillRuns:true}));})));
 await Effect.runPromise(Effect.scoped(Effect.gen(function*(){const before=calls.length;const r=recovery(true);yield* OpenCode.create(opts,{overrides:[r.replacement,...taps]});yield* Effect.promise(()=>r.ready);assert.equal(calls.length,before);console.log(JSON.stringify({case:'OpenCode-second-clean-reopen',noModelCall:true}));})));
}finally{server.close()}
