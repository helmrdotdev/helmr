import { describe, expect, test } from "bun:test"
import { validateSecretName } from "./secret"
import { encodeComputerSecrets } from "./computer"

const secretID = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc38"

describe("Computer Secret bindings", () => {
 test("stable identities and explicit modes encode without wrappers", () => {
  expect(encodeComputerSecrets([{secretId: secretID, env: {name: "GH_TOKEN", mode: "protected", allowedOrigins: ["HTTPS://API.GITHUB.COM:443/", "https://api.github.com"]}}])).toEqual([
   {secretId: secretID, env: {name: "GH_TOKEN", mode: "protected", allowedOrigins: ["https://api.github.com"]}},
  ])
 })
 test("rejects names outside the Control contract", () => {
  for (const name of ["", "-bad", "bad/name", "bad name", "a".repeat(129)]) expect(() => validateSecretName(name)).toThrow("Secret name is invalid")
 })
 test("mode and exactly one delivery are required", () => {
  for (const binding of [
   {secretId:secretID,env:{name:"TOKEN"}},
   {secretId:secretID,env:{name:"TOKEN",mode:"raw",allowedOrigins:[]}},
   {secretId:secretID,env:{name:"TOKEN",mode:"protected",allowedOrigins:[]}},
   {secretId:secretID,env:{name:"TOKEN",mode:"raw"},file:{path:"/run/secrets/key"}},
  ]) expect(() => encodeComputerSecrets([binding as never])).toThrow()
 })
 test("mixed raw and protected placements are deliberate per binding", () => {
  expect(encodeComputerSecrets([
   {secretId:secretID,env:{name:"TOKEN",mode:"raw"}},
   {secretId:secretID,env:{name:"PROTECTED_TOKEN",mode:"protected",allowedOrigins:["https://api.github.com"]}},
  ])).toHaveLength(2)
 })
})

test("optional undefined binding members are absent; unknown members remain invalid", () => {
 expect(encodeComputerSecrets([{secretId:secretID,env:{name:"TOKEN",mode:"raw",allowedOrigins:undefined},file:undefined}])).toHaveLength(1)
 expect(encodeComputerSecrets([{secretId:secretID,file:{path:"/run/secrets/key"},env:undefined}])).toHaveLength(1)
 expect(() => encodeComputerSecrets([{secretId:secretID,env:{name:"TOKEN",mode:"raw",typo:undefined}} as never])).toThrow("unknown")
})
test("managed env names match server and guest", async () => {
 const vectors = await Bun.file(new URL("../../../internal/secretbinding/testdata/secret-env-names.json",import.meta.url)).json()
 for (const vector of vectors) {
  const encode = () => encodeComputerSecrets([{secretId:secretID,env:{name:vector.name,mode:"raw"}}])
  if(vector.reserved) expect(encode).toThrow()
  else expect(encode()).toHaveLength(1)
 }
})
test("aggregate origins count normalized entries per binding, including ports", () => {
 for(const variant of ["distinct","repeated","ports","dedup"]) {
  const bindings = Array.from({length:16},(_,i)=>({secretId:secretID,env:{name:`TOKEN_${i}`,mode:"protected" as const,allowedOrigins:Array.from({length:16},(_,j)=>variant==="dedup"?"https://EXAMPLE.com:443/":variant==="ports"?`https://example.com:${1000+j}`:`https://h${variant==="repeated"?j:i*16+j}.example.com`)}}))
  expect(encodeComputerSecrets(bindings)).toHaveLength(16)
  bindings.push({secretId:secretID,env:{name:"EXTRA",mode:"protected",allowedOrigins:["https://extra.example.com"]}})
  if(variant==="dedup") expect(encodeComputerSecrets(bindings)).toHaveLength(17)
  else expect(()=>encodeComputerSecrets(bindings)).toThrow("256")
 }
})

test("Secret file paths use the server UTF-8 byte limit", () => {
 const path = "/" + "é".repeat(2047) + "a"
 expect(encodeComputerSecrets([{secretId:secretID,file:{path}}])).toHaveLength(1)
 expect(() => encodeComputerSecrets([{secretId:secretID,file:{path:path+"b"}}])).toThrow("file path")
})
