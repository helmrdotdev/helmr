import assert from 'node:assert/strict';
import {Type} from 'typebox';
import {mkdirSync,readFileSync,writeFileSync} from 'node:fs';
import {createAssistantMessageEventStream} from '@earendil-works/pi-ai';
import {createAgentSession,createExtensionRuntime,ModelRuntime,SessionManager,SettingsManager} from '@earendil-works/pi-coding-agent';
const root='/workspace/native-sdk/isolated/pi';
mkdirSync(root+'/sessions',{recursive:true});
let toolExecutions=0;let toolMode=true, toolStarted:any, releaseTool:any, toolEnded=false; let contexts:any[]=[]; let calls=0, waitMode=false, startedResolve:any;
const runtime=await ModelRuntime.create({authPath:root+'/auth.json',modelsPath:null,modelsStorePath:root+'/models-store.json',allowModelNetwork:false,refreshOnCreate:false});
runtime.registerProvider('probe',{baseUrl:'https://invalid.example',api:'probe-api',apiKey:'offline-fixture',models:[{id:'probe',name:'probe',reasoning:false,input:['text'],cost:{input:0,output:0,cacheRead:0,cacheWrite:0},contextWindow:100000,maxTokens:1000}],streamSimple:(model,context,options)=>{
 calls++; contexts.push(JSON.stringify(context.messages));
 const stream=createAssistantMessageEventStream();
 const msg:any={role:'assistant',content:[{type:'text',text:'fixture reply'}],api:model.api,provider:model.provider,model:model.id,usage:{input:1,output:1,cacheRead:0,cacheWrite:0,totalTokens:2,cost:{input:0,output:0,cacheRead:0,cacheWrite:0,total:0}},stopReason:'stop',timestamp:Date.now()};
 if(toolMode){msg.content=[{type:'toolCall',id:'tool-fixture',name:'probeTool',arguments:{}}];msg.stopReason='toolUse';toolMode=false;}
 if(waitMode){
  options?.signal?.addEventListener('abort',()=>{msg.stopReason='aborted';msg.errorMessage='fixture aborted';stream.push({type:'error',reason:'aborted',error:msg});stream.end(msg)},{once:true});
  startedResolve?.();
 }else setTimeout(()=>{stream.push({type:'done',reason:msg.stopReason,message:msg});stream.end(msg)},5);
 return stream;
}});
const model=runtime.getModel('probe','probe');assert.ok(model);
const resourceLoader:any={getExtensions:()=>({extensions:[],errors:[],runtime:createExtensionRuntime()}),getSkills:()=>({skills:[],diagnostics:[]}),getPrompts:()=>({prompts:[],diagnostics:[]}),getThemes:()=>({themes:[],diagnostics:[]}),getAgentsFiles:()=>({agentsFiles:[]}),getSystemPrompt:()=>'',getSystemPromptSource:()=>undefined,getAppendSystemPrompt:()=>[],getAppendSystemPromptSources:()=>[],extendResources:()=>{},reload:async()=>{}};
const manager=SessionManager.create(root,root+'/sessions');
const create=async(sm:any)=>createAgentSession({cwd:root,agentDir:root,model,modelRuntime:runtime,thinkingLevel:'off',resourceLoader,tools:['probeTool'],customTools:[{name:'probeTool',label:'probe',description:'offline tool',parameters:Type.Object({}),execute:async()=>{toolExecutions++;toolStarted?.();await new Promise(r=>releaseTool=r);toolEnded=true;return {content:[{type:'text',text:'released'}],details:{}}}}],sessionManager:sm,settingsManager:SettingsManager.inMemory({compaction:{enabled:false},retry:{enabled:false},cacheWarming:{mode:'off'}} as any)});
const {session}=await create(manager);session.setActiveToolsByName(['probeTool']);
const started=new Promise(r=>toolStarted=r);const running=session.prompt('run held tool');await started;const saved=manager.getSessionFile();const incomplete=readFileSync(saved,'utf8');
let abortSettled=false;const abort=session.abort().then(()=>abortSettled=true);await new Promise(r=>setTimeout(r,100));assert.equal(abortSettled,false);assert.equal(toolEnded,false);releaseTool();await abort;await running;assert.equal(session.isIdle,true);assert.equal(toolEnded,true);assert.equal(calls,1);
await session.prompt('new input after tool stop');assert.equal(calls,2);session.dispose();const orphan=root+'/sessions/incomplete-'+Date.now()+'.jsonl';writeFileSync(orphan,incomplete);const before=calls;const {session:restored}=await create(SessionManager.open(orphan));assert.equal(calls,before);await restored.prompt('fresh after incomplete tool history');assert.equal(calls,before+1);assert.equal(toolExecutions,1);restored.dispose();console.log(JSON.stringify({case:'Pi-held-tool-abort',abortWaitsForIgnoringTool:true,toolSettledBeforeIdle:true,noPostAbortContinuation:true,newInputWorks:true,incompleteToolHistoryOpenInert:true,newInputDoesNotReplayTool:true}));
