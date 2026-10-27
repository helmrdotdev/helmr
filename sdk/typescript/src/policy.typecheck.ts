import { agent, template, image, type NetworkPolicy, type RuntimeMcpPolicy, type PrepareContext } from "./index"
const network: NetworkPolicy = {allowDomains:["api.example",{domain:"*.corp.example",ipRanges:["10.0.0.0/8"]}]}
const runtimeMcp: RuntimeMcpPolicy = {allow:[{operation:"spawn",agents:["worker"]},{operation:"inspect_turn",agents:["worker"]},{operation:"list_computers"}]}
const machine=template({id:"machine",image:image("base").from("debian:bookworm-slim"),resources:{cpu:1,memory:"1GiB"},network,prepare:async (ctx:PrepareContext)=>{await ctx.exec("true")},prepareNetwork:{blockAll:true},prepareSecrets:[]})
agent({id:"worker",template:machine,runtimeMcp,init:()=>({answer:42}),turn:(_turn,ctx)=>ctx.initResult.answer})
// @ts-expect-error network branches are mutually exclusive
const mixed:NetworkPolicy={blockAll:false,allowDomains:[]}
// @ts-expect-error explicit MCP requires allow
const missing:RuntimeMcpPolicy={}
// @ts-expect-error MCP has no blockAll
const off:RuntimeMcpPolicy={blockAll:true}
// @ts-expect-error targeted operations do not accept computer placement
const placement:RuntimeMcpPolicy={allow:[{operation:"enqueue",agents:["worker"],computer:"new"}]}
// @ts-expect-error list_computers has no Agent targets
const discovery:RuntimeMcpPolicy={allow:[{operation:"list_computers",agents:["worker"]}]}
// @ts-expect-error preparation Secret name is prepareSecrets
template({id:"old",image:image("old").from("debian"),resources:{cpu:1,memory:"1GiB"},buildSecrets:[]})
void [mixed,missing,off,placement,discovery]
