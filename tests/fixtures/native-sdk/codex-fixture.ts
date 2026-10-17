import {spawn} from 'node:child_process';
import {createInterface} from 'node:readline';
import {mkdirSync,writeFileSync} from 'node:fs';
export const root='/workspace/native-sdk';
export const base=root+'/isolated/codex';mkdirSync(base,{recursive:true});
export const bin=root+'/node_modules/@openai/codex-linux-x64/vendor/x86_64-unknown-linux-musl/bin/codex';
export const delay=(ms:number)=>new Promise(r=>setTimeout(r,ms));
export async function until(p:()=>boolean,label='condition'){for(let n=0;n<700;n++){if(p())return;await delay(20)}throw Error('timeout: '+label)}
export const requests:any[]=[],paths:any[]=[],held:any[]=[];let nextActions:any[]=[];let socketCount=0;
export const setActions=(...actions:any[])=>{nextActions.push(...actions)};
export const tool=(name:string,args:any,id='call_'+crypto.randomUUID())=>({type:'function_call',call_id:id,name,arguments:JSON.stringify(args)});
export const message=(text='offline reply')=>({type:'message',role:'assistant',id:'msg_'+crypto.randomUUID(),content:[{type:'output_text',text}]});
function events(item:any){const id='resp_'+crypto.randomUUID();return [{type:'response.created',response:{id}},...(item?[{type:'response.output_item.done',item}]:[]),{type:'response.completed',response:{id,output:item?[item]:[],usage:{input_tokens:10,output_tokens:2,total_tokens:12}}}]}
function handle(body:any,send:any,end:any){requests.push(body);if(body.generate===false){for(const e of events(null))send(e);end();return}const action=nextActions.shift()??message();if(action==='hold'){held.push({body,send,end,respond:(item=message())=>{for(const e of events(item))send(e);end()}});return}for(const e of events(action))send(e);end()}
export const server=Bun.serve({hostname:'127.0.0.1',port:18479,fetch:async(req,srv)=>{const url=new URL(req.url);paths.push({path:url.pathname,method:req.method,upgrade:req.headers.get('upgrade')});if(req.headers.get('upgrade')==='websocket'){if(srv.upgrade(req))return;return new Response('failed',{status:400})}if(url.pathname.endsWith('/responses')){const body=await req.json();return new Response(new ReadableStream({start(c){handle(body,(e:any)=>c.enqueue(new TextEncoder().encode('data: '+JSON.stringify(e)+'\n\n')),()=>c.close())}}),{headers:{'content-type':'text/event-stream'}})}return Response.json({data:[]})},websocket:{open(){socketCount++},message(ws,data){handle(JSON.parse(data.toString()),(e:any)=>ws.send(JSON.stringify(e)),()=>{})}}});
export function transport(){return {socketCount,paths}}
export class Client{
 proc:any;events:any[]=[];pending=new Map<number,any>();seq=0;stderr='';serverRequests:any[]=[];
 constructor(public home:string,public useWS=true,extraConfig='',goals=true){
 mkdirSync(home,{recursive:true});writeFileSync(home+'/config.toml',`model = "gpt-5.1-codex"\nmodel_provider = "fixture"\nmodel_reasoning_effort = "low"\n[model_providers.fixture]\nname = "Offline fixture"\nbase_url = "http://127.0.0.1:18479/v1"\nwire_api = "responses"\nenv_key = "OFFLINE_FIXTURE_KEY"\nrequires_openai_auth = false\nsupports_websockets = ${useWS}\n${extraConfig}\n[features]\ngoals = true\nmulti_agent = true\nmulti_agent_v2 = false\n`);
 this.proc=spawn(bin,['-c','features.goals='+goals,'-c','features.multi_agent=true','-c','features.multi_agent_v2=false','app-server','--listen','stdio://'],{cwd:base,env:{...process.env,CODEX_HOME:home,HOME:base,OFFLINE_FIXTURE_KEY:'offline-only',RUST_LOG:'error'},stdio:['pipe','pipe','pipe']});
 this.proc.stderr.on('data',(b:any)=>{this.stderr=(this.stderr+b.toString()).slice(-4000);console.error('CODEX_STDERR',new Date().toISOString(),b.toString().slice(0,4000))});this.proc.on('error',(error:any)=>{console.error('CODEX_SPAWN_ERROR',String(error));for(const p of this.pending.values())p.reject(error);this.pending.clear()});this.proc.on('exit',(code:any,signal:any)=>{console.error('CODEX_EXIT',new Date().toISOString(),{code,signal});for(const p of this.pending.values())p.reject(Error('Codex exited '+JSON.stringify({code,signal,stderr:this.stderr})));this.pending.clear()});createInterface({input:this.proc.stdout}).on('line',line=>{let m;try{m=JSON.parse(line)}catch{return}this.events.push(m);if(m.method&&m.id!==undefined){this.serverRequests.push(m);return}if(m.id!==undefined&&this.pending.has(m.id)){const p=this.pending.get(m.id);this.pending.delete(m.id);m.error?p.reject(Object.assign(Error(m.error.message),{rpc:m.error})):p.resolve(m.result)}});
 }
 send(m:any){this.proc.stdin.write(JSON.stringify(m)+'\n')}
 call(method:string,params:any={}){console.error('PHASE codex rpc',new Date().toISOString(),method);const id=++this.seq;return new Promise<any>((resolve,reject)=>{this.pending.set(id,{resolve,reject});this.send({id,method,params})})}
 reply(id:any,result:any){this.send({id,result})}
 async init(){const r=await this.call('initialize',{clientInfo:{name:'offline_lifecycle_probe',version:'1'},capabilities:{experimentalApi:true}});this.send({method:'initialized',params:{}});return r}
 async start(opts:any={}){return this.call('thread/start',{model:'gpt-5.1-codex',modelProvider:'fixture',cwd:base,approvalPolicy:'never',sandbox:'danger-full-access',baseInstructions:'You are an offline test agent.',...opts})}
 turn(id:string,text:string,opts:any={}){return this.call('turn/start',{threadId:id,input:[{type:'text',text}],...opts})}
 async done(id:string){await until(()=>this.events.some(e=>e.method==='turn/completed'&&e.params.turn.id===id),'turn '+id+' '+this.stderr.slice(-300));return this.events.find(e=>e.method==='turn/completed'&&e.params.turn.id===id).params.turn}
 async close(){this.proc.stdin.end();await delay(200);if(this.proc.exitCode===null&&this.proc.signalCode===null)this.proc.kill('SIGINT');await until(()=>this.proc.exitCode!==null||this.proc.signalCode!==null,'owner exit')}
}
