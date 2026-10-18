import assert from 'node:assert/strict';
import {Type} from 'typebox';
import {mkdirSync,existsSync,readFileSync,unlinkSync} from 'node:fs';
import {createAssistantMessageEventStream} from '@earendil-works/pi-ai';
import {createAgentSession,createExtensionRuntime,ModelRuntime,SessionManager,SettingsManager} from '@earendil-works/pi-coding-agent';
const root='/workspace/native-sdk/isolated/pi';
mkdirSync(root+'/sessions',{recursive:true});
let toolMode=true, toolStarted:any, releaseTool:any, toolEnded=false; let contexts:any[]=[]; let calls=0, waitMode=false, startedResolve:any;
const runtime=await ModelRuntime.create({authPath:root+'/auth.json',modelsPath:null,modelsStorePath:root+'/models-store.json',allowModelNetwork:false,refreshOnCreate:false});
runtime.registerProvider('probe',{baseUrl:'https://invalid.example',api:'probe-api',apiKey:'offline-fixture',models:[{id:'probe',name:'probe',reasoning:false,input:['text'],cost:{input:0,output:0,cacheRead:0,cacheWrite:0},contextWindow:100000,maxTokens:1000}],streamSimple:(model,context,options)=>{
 calls++; contexts.push(JSON.stringify(context.messages));
 const stream=createAssistantMessageEventStream();
 const msg:any={role:'assistant',content:[{type:'text',text:'fixture reply'}],api:model.api,provider:model.provider,model:model.id,usage:{input:1,output:1,cacheRead:0,cacheWrite:0,totalTokens:2,cost:{input:0,output:0,cacheRead:0,cacheWrite:0,total:0}},stopReason:'stop',timestamp:Date.now()};
 if(toolMode){msg.content=[{type:'toolCall',id:'tool-fixture',name:'bash',arguments:{command:'echo $$ > /workspace/native-sdk/isolated/pi-tool-owner; /bin/sleep 30; echo done > /workspace/native-sdk/isolated/pi-tool-finished'}}];msg.stopReason='toolUse';toolMode=false;}
 if(waitMode){
  options?.signal?.addEventListener('abort',()=>{msg.stopReason='aborted';msg.errorMessage='fixture aborted';stream.push({type:'error',reason:'aborted',error:msg});stream.end(msg)},{once:true});
  startedResolve?.();
 }else setTimeout(()=>{stream.push({type:'done',reason:msg.stopReason,message:msg});stream.end(msg)},5);
 return stream;
}});
const model=runtime.getModel('probe','probe');assert.ok(model);
const resourceLoader:any={getExtensions:()=>({extensions:[],errors:[],runtime:createExtensionRuntime()}),getSkills:()=>({skills:[],diagnostics:[]}),getPrompts:()=>({prompts:[],diagnostics:[]}),getThemes:()=>({themes:[],diagnostics:[]}),getAgentsFiles:()=>({agentsFiles:[]}),getSystemPrompt:()=>'',getSystemPromptSource:()=>undefined,getAppendSystemPrompt:()=>[],getAppendSystemPromptSources:()=>[],extendResources:()=>{},reload:async()=>{}};
const manager=SessionManager.create(root,root+'/sessions');
const create=async(sm:any)=>createAgentSession({cwd:root,agentDir:root,model,modelRuntime:runtime,thinkingLevel:'off',resourceLoader,tools:['bash'],sessionManager:sm,settingsManager:SettingsManager.inMemory({compaction:{enabled:false},retry:{enabled:false},cacheWarming:{mode:'off'}} as any)});
const marker='/workspace/native-sdk/isolated/pi-tool-owner',finished='/workspace/native-sdk/isolated/pi-tool-finished';for(const p of [marker,finished])if(existsSync(p))unlinkSync(p);
const until=async(p:any)=>{for(let n=0;n<500;n++){if(p())return;await new Promise(r=>setTimeout(r,20))}throw Error('timeout')};
const {session}=await create(manager);const running=session.prompt('run native bash');await until(()=>existsSync(marker));const pid=Number(readFileSync(marker,'utf8').trim());await session.abort();await running;await until(()=>{try{process.kill(pid,0);return false}catch{return true}});assert.equal(session.isIdle,true);assert.equal(calls,1);assert.equal(existsSync(finished),false);await session.prompt('new input after native tool abort');assert.equal(calls,2);session.dispose();console.log(JSON.stringify({case:'Pi-native-bash-abort',joined:true,toolShellGone:true,toolDidNotFinish:true,noModelContinuation:true,newInputWorks:true}));
