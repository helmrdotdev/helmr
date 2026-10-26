import { expect,test } from "bun:test"
import { HelmrClient } from "./client"
const deploymentId="019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31"
test("definition reads expose normalized pinned policies through get and list",async()=>{
 let response:unknown={}
 const client=new HelmrClient({apiKey:"key",fetch:(async()=>Response.json(response)) as typeof fetch})
 const template={template:"machine",resources:{milliCpu:1000,memoryMiB:1024},network:{blockAll:false},prepare_network:{blockAll:true},prepare_secrets:[{secret:"SOURCE_TOKEN",file:{path:"/tmp/token"}}]}
 response={...template,deployment_id:deploymentId}
 expect(await client.templates.get("machine")).toEqual({template:"machine",deploymentId,resources:template.resources,network:{blockAll:false},prepareNetwork:{blockAll:true},prepareSecrets:template.prepare_secrets})
 response={templates:[template],deployment_id:deploymentId}
 expect((await client.templates.list()).items[0]?.prepareNetwork).toEqual({blockAll:true})
 response={agent:"worker",template:"machine",input_kind:"text",runtime_mcp:{allow:[]},deployment_id:deploymentId}
 expect((await client.agents.get("worker")).runtimeMcp).toEqual({allow:[]})
 response={agents:[{agent:"worker",template:"machine",input_kind:"text",runtime_mcp:{allow:[{operation:"spawn",agents:["reviewer"]}]}}],deployment_id:deploymentId}
 expect((await client.agents.list()).items[0]?.runtimeMcp).toEqual({allow:[{operation:"spawn",agents:["reviewer"],computer:"new"}]})
 for (const runtime_mcp of [null,{},false,{blockAll:true}]) {response={agent:"worker",template:"machine",input_kind:"text",runtime_mcp,deployment_id:deploymentId};await expect(client.agents.get("worker")).rejects.toThrow()}
 for(const missing of ["network","prepare_network","prepare_secrets"]) {const invalid:Record<string,unknown>={...template,deployment_id:deploymentId};delete invalid[missing];response=invalid;await expect(client.templates.get("machine")).rejects.toThrow()}
})
