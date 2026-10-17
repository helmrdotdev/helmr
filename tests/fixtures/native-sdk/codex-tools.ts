import assert from 'node:assert/strict';
import {existsSync,readFileSync,unlinkSync} from 'node:fs';
import {Client,base,requests,server,setActions,tool,until,held,delay} from './codex-fixture';
const c=new Client(base+'/tools-'+Date.now(),true,'',false);const pids:number[]=[];
function alive(pid:number){try{process.kill(pid,0);return true}catch{return false}}
try{await c.init();const {thread}=await c.start();const results:any[]=[];
for(const background of [false,true]){
 const marker=base+'/tool-'+background,finished=marker+'-finished';for(const p of [marker,finished])if(existsSync(p))unlinkSync(p);
 setActions(tool('exec_command',{cmd:'echo $$ > '+marker+'; /bin/sleep 30; echo done > '+finished,login:false,yield_time_ms:background?1000:10000}));if(background)setActions('hold');const {turn}=await c.turn(thread.id,'offline tool '+background);await until(()=>existsSync(marker),'shell started');const pid=Number(readFileSync(marker,'utf8').trim());pids.push(pid);if(background)await until(()=>held.length>0,'background returned and model active');
 await c.call('turn/interrupt',{threadId:thread.id,turnId:turn.id});assert.equal((await c.done(turn.id)).status,'interrupted');await delay(150);const aliveAfterInterrupt=alive(pid);const terminals=await c.call('thread/backgroundTerminals/list',{threadId:thread.id});await c.call('thread/backgroundTerminals/clean',{threadId:thread.id});await until(()=>!alive(pid),'tool gone after clean');assert.equal(existsSync(finished),false);const before=requests.filter(r=>r.generate!==false).length;await delay(150);assert.equal(requests.filter(r=>r.generate!==false).length,before);results.push({background,aliveAfterInterrupt,terminals});const next=await c.turn(thread.id,'fresh after tool cancellation');assert.equal((await c.done(next.turn.id)).status,'completed');}
 console.log(JSON.stringify({case:'Codex-native-tools-interrupt-clean',results,newInputWorks:true,noLateCompletion:true}));
}finally{await c.close();for(const pid of pids)if(alive(pid))try{process.kill(-pid,'SIGKILL')}catch{try{process.kill(pid,'SIGKILL')}catch{}}server.stop(true)}
