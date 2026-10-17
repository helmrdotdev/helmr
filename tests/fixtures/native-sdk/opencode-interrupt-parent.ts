import assert from 'node:assert/strict';
import {spawn} from 'node:child_process';
import {existsSync,readFileSync,unlinkSync} from 'node:fs';
import {calls,server} from './mock-tool-http';
const root='/workspace/native-sdk';const marker=root+'/isolated/tool-owner';const finished=root+'/isolated/tool-finished';for(const p of [marker,finished])if(existsSync(p))unlinkSync(p);
const until=async(p:any)=>{for(let i=0;i<500;i++){if(p())return;await new Promise(r=>setTimeout(r,20))}throw Error('timeout '+JSON.stringify({calls:calls.length,children:children.map(c=>({pid:c.pid,exitCode:c.exitCode,signalCode:c.signalCode}))}))};
const children:any[]=[];function start(mode:string){const c=spawn(process.execPath,[root+'/opencode-interrupt-child.ts',mode],{cwd:root,env:process.env,detached:true,stdio:['ignore','pipe','pipe']});children.push(c);c.stdout.on('data',b=>console.log('CHILD',b.toString().trim()));c.stderr.on('data',b=>console.log('STDERR',b.toString().slice(0,1000)));return c}
try{const first=start('interrupt');await until(()=>first.exitCode!==null||first.signalCode!==null);assert.equal(first.exitCode,0);assert.equal(existsSync(finished),false);console.log(JSON.stringify({case:'OpenCode-actual-native-interrupt',completionMarkerAbsentAtStop:true}));}finally{for(const c of children)if(c.exitCode===null&&c.signalCode===null)try{process.kill(-c.pid,'SIGKILL')}catch{}server.close()}
