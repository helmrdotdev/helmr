import { expect, test } from "bun:test"
import { analyze } from "./compile"
import { image, template, agent } from "../../../sdk/typescript/src/index"
import { normalizeNetwork, normalizeRuntimeMcp } from "./policy"

const keys = new Set(["worker","reviewer"])
test("network grammar normalizes exact, wildcard, and bounded domain grants", () => {
  expect(normalizeNetwork({allowDomains:["API.Example.com.",{domain:"*.corp.example",ipRanges:["10.60.0.0/16","10.40.0.0/16"]}]},"network")).toEqual({allowDomains:[{domain:"*.corp.example",ipRanges:["10.40.0.0/16","10.60.0.0/16"]},"api.example.com"]})
  expect(normalizeNetwork({allowDomains:[]},"network")).toEqual({blockAll:true})
  for (const value of [{},null,true,{blockAll:false,allowDomains:[]},{blockAll:true,x:1},{allowDomains:["*.com"]},{allowDomains:["api.example","API.EXAMPLE."]},{allowDomains:["localhost"]},{allowDomains:["https://api.example"]},{allowDomains:["127.0.0.1"]},{allowDomains:["é.example"]},{allowDomains:[{domain:"api.example",ipRanges:[]}]},{allowDomains:[{domain:"api.example",ipRanges:["10.0.0.1/8"]}]},{allowDomains:Array(257).fill("api.example")}]) expect(()=>normalizeNetwork(value,"network")).toThrow("invalid_network_policy")
})
test("MCP finite grants validate manifest keys, placement, and strict members",()=>{
 expect(normalizeRuntimeMcp({allow:[{operation:"spawn",agents:["reviewer"]},{operation:"spawn",agents:["worker"]},{operation:"list_computers"}]},keys,"runtimeMcp")).toEqual({allow:[{operation:"list_computers"},{operation:"spawn",agents:["reviewer","worker"],computer:"new"}]})
 expect(normalizeRuntimeMcp({allow:[]},keys,"runtimeMcp")).toEqual({allow:[]})
 for(const value of [{},null,true,{blockAll:true},{blockAll:false},{allow:[],extra:1},{allow:[{operation:"spawn",agents:[]}]},{allow:[{operation:"start",agents:["missing"]}]},{allow:[{operation:"enqueue",agents:["worker"],computer:"new"}]},{allow:[{operation:"list_computers",agents:[]}]},{allow:[{operation:"wait_turn",agents:["worker"]}]},{allow:[{operation:"start",agents:["worker","worker"]}]}]) expect(()=>normalizeRuntimeMcp(value,keys,"runtimeMcp")).toThrow("invalid_runtime_mcp_policy")
})
function compile(value:unknown) {return analyze({architecture:"x86_64",exports:[{modulePath:"helmr/app/entry-0.mjs",exportName:"worker",value}]})}
const base = () => template({id:"machine",image:image("base").from("debian:bookworm-slim"),resources:{cpu:1,memory:"1GiB"}})
test("compiler validates preparation phase and copied policy metadata",()=>{
 for(const config of [{prepareNetwork:{blockAll:true}},{prepareSecrets:[{secret:"TOKEN",file:{path:"/tmp/token"}}]}]) expect(()=>compile({...base(),...config})).toThrow("invalid_network_policy")
 expect(()=>compile({...base(),buildSecrets:[]})).toThrow("renamed to prepareSecrets")
 const domains=["API.Example."]
 const machine={...base(),network:{allowDomains:domains},prepareNetwork:{blockAll:true},prepare:async()=>{}}
 const worker=agent({id:"worker",template:machine, runtimeMcp:{allow:[{operation:"spawn",agents:["worker"]}]},turn:async()=>null})
 const output=compile(worker).buildPlan
 domains.push("other.example")
 expect(output.definitions[1]?.manifest).toMatchObject({network:{allowDomains:["api.example"]},prepareNetwork:{blockAll:true},prepareSecrets:[]})
 expect(output.definitions[0]?.manifest).toMatchObject({runtimeMcp:{allow:[{operation:"spawn",agents:["worker"],computer:"new"}]}})
 expect(()=>compile({...base(),network:{blockAll:true},secrets:[{secret:"TOKEN",env:{name:"TOKEN",mode:"protected",allowedOrigins:["https://api.example"]}}]})).toThrow("invalid_network_policy")
})


test("MCP authored aliases explain their derived grants before requiring targets", () => {
  for (const [operation, hint] of [
    ["list_agents", "list_agents is derived from start/spawn grants; author start or spawn instead"],
    ["skills", "skills is local guidance derived from descriptor capabilities, not an authored business operation"],
    ["list_sessions", "list_sessions is derived from inspect_session; author inspect_session instead"],
    ["wait_turn", "wait_turn is derived from inspect_turn; author inspect_turn instead"],
  ] as const) {
    for (const rule of [{ operation }, { operation, agents: [] }, { operation, agents: null }, { operation, agents: ["worker"] }]) {
      expect(() => normalizeRuntimeMcp({ allow: [rule] }, keys, "worker.runtimeMcp")).toThrow(
        `invalid_runtime_mcp_policy worker.runtimeMcp.allow[0].operation: ${hint}`,
      )
    }
  }
  for (const operation of [undefined, null, false, {}, "unknown_operation"]) {
    expect(() => normalizeRuntimeMcp({ allow: [{ operation }] }, keys, "worker.runtimeMcp")).toThrow(
      "invalid_runtime_mcp_policy worker.runtimeMcp.allow[0].operation: unsupported operation",
    )
  }
  expect(() => normalizeRuntimeMcp({ allow: [{ operation: "spawn" }] }, keys, "worker.runtimeMcp")).toThrow(".agents: expected 1 to 256 unique Agent keys")
  expect(() => normalizeRuntimeMcp({ allow: [{ operation: "skills", typo: true }] }, keys, "worker.runtimeMcp")).toThrow("invalid or unknown members")
})
