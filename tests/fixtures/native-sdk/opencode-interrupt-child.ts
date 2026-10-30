import assert from 'node:assert/strict';
import {writeFileSync,readFileSync,existsSync} from 'node:fs';
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
const config={permissions:[{action:'*',resource:'*',effect:'allow'}],model:'fixture/probe',providers:{fixture:{name:'Offline fixture',package:'@opencode/ai/providers/anthropic',settings:{baseURL:'http://127.0.0.1:18479/v1',apiKey:'offline-fixture'},models:{probe:{name:'Probe',limit:{context:100000,output:1000},capabilities:{tools:true,input:['text'],output:['text']}}}}}};
const opts={database:{path:root+'/crash-native.sqlite'},config:{directory:root+'/config',project:false,claude:false,content:JSON.stringify(config)},models:{fetch:false,snapshot:false},fs:{filewatcher:false,fff:false},plugins:[]};
let retired=0;
function recovery(cancel:boolean){let ok:any,bad:any;const ready=new Promise((r,j)=>{ok=r;bad=j});
 const replacement=SessionRestart.node.replace(SessionRestart.node.mapLayer(()=>Layer.unwrap(Effect.gen(function*(){
  const original=yield* Session.Service;
  const nativeLayer=SessionRestart.layer({maxAttempts:0}).pipe(Layer.provide(Layer.succeed(Session.Service,{...original,synthetic:(input:any)=>original.synthetic({...input,resume:false})})));
  return Layer.effect(SessionRestart.Service,Effect.gen(function*(){const native=yield* SessionRestart.Service;return {resumeSuspendedSessions:Effect.gen(function*(){yield* native.resumeSuspendedSessions;if(cancel){for(const id of ids){for(const row of yield* original.inbox(id)){yield* original.cancelInbox({sessionID:id,inboxID:row.id});retired++}}}}).pipe(Effect.onExit(exit=>Effect.sync(()=>exit._tag==='Success'?ok():bad(exit))))}})).pipe(Layer.provide(nativeLayer));
 }))));return {replacement,ready};}
await Effect.runPromise(Effect.scoped(Effect.gen(function*(){
 if(process.argv[2]==='start'||process.argv[2]==='interrupt'){console.error('PHASE opencode create',new Date().toISOString());const sdk=yield* OpenCode.create(opts,{overrides:[inert,...taps]});const p=yield* sdk.sessions.create({location:{directory:root}});writeFileSync(root+'/crash-id.json',JSON.stringify([p.id]));console.error('PHASE opencode prompt',new Date().toISOString());yield* sdk.sessions.prompt({sessionID:p.id,text:'run offline held shell'});if(process.argv[2]==='start')yield* Effect.never;else{yield* Effect.promise(async()=>{for(let n=0;n<500;n++){if(existsSync(root+'/tool-owner'))return;await new Promise(r=>setTimeout(r,20))}throw Error('tool did not start')});const pid=Number(readFileSync(root+'/tool-owner','utf8').trim());const interrupted=yield* execution.interrupt(p.id,{resume:false});assert.equal(interrupted,true);yield* execution.awaitIdle(p.id);yield* Effect.promise(async()=>{for(let n=0;n<100;n++){try{process.kill(pid,0)}catch{return}await new Promise(r=>setTimeout(r,20))}throw Error('tool still alive')});console.error('PHASE opencode prompt',new Date().toISOString());yield* sdk.sessions.prompt({sessionID:p.id,text:'fresh after native interrupt'});yield* execution.awaitIdle(p.id);{const messages=yield* store.messages({sessionID:p.id,order:'desc'});assert.equal(messages.find((x:any)=>x.type==='idle')?.outcome,'succeeded');assert.ok(messages.find((x:any)=>x.type==='assistant')?.content.some((x:any)=>x.type==='text'&&x.text==='offline reply'));}console.log(JSON.stringify({case:'OpenCode-native-shell-interrupt',toolShellGone:true,newInputWorks:true}));}}
 else{ids=JSON.parse(readFileSync(root+'/crash-id.json','utf8'));const r=recovery(true);console.error('PHASE opencode create',new Date().toISOString());const sdk=yield* OpenCode.create(opts,{overrides:[r.replacement,...taps]});yield* Effect.promise(()=>r.ready);assert.equal((yield* store.listSuspended()).length,0);console.log('RECOVERY_READY');yield* Effect.sleep('200 millis');console.error('PHASE opencode prompt',new Date().toISOString());yield* sdk.sessions.prompt({sessionID:ids[0],text:'fresh after killed native tool'});yield* execution.awaitIdle(ids[0]);console.log(JSON.stringify({case:'OpenCode-real-killed-tool-recovery',retired,messages:(yield* sessions.messages({sessionID:ids[0]})).map((x:any)=>({type:x.type,error:x.error,parts:x.parts}))}));}
})));
