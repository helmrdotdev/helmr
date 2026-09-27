import { describe, expect, test } from "bun:test"
import { validateSecretName } from "./secret"
import { encodeComputerSecrets } from "./computer"

describe("Computer Secret bindings", () => {
 test("plain names and explicit modes encode without wrappers", () => {
  expect(encodeComputerSecrets([{secret: "token", env: {name: "GH_TOKEN", mode: "protected", allowedOrigins: ["HTTPS://API.GITHUB.COM:443/", "https://api.github.com"]}}])).toEqual([
   {secret: "token", env: {name: "GH_TOKEN", mode: "protected", allowed_origins: ["https://api.github.com"]}},
  ])
 })
 test("rejects names outside the Control contract", () => {
  for (const name of ["", "-bad", "bad/name", "bad name", "a".repeat(129)]) expect(() => validateSecretName(name)).toThrow("Secret name is invalid")
 })
 test("mode and exactly one delivery are required", () => {
  for (const binding of [
   {secret:"token",env:{name:"TOKEN"}},
   {secret:"token",env:{name:"TOKEN",mode:"raw",allowedOrigins:[]}},
   {secret:"token",env:{name:"TOKEN",mode:"protected",allowedOrigins:[]}},
   {secret:"token",env:{name:"TOKEN",mode:"raw"},file:{path:"/run/secrets/key"}},
  ]) expect(() => encodeComputerSecrets([binding as never])).toThrow()
 })
 test("mixed raw and protected placements are deliberate per binding", () => {
  expect(encodeComputerSecrets([
   {secret:"token",env:{name:"TOKEN",mode:"raw"}},
   {secret:"token",env:{name:"PROTECTED_TOKEN",mode:"protected",allowedOrigins:["https://api.github.com"]}},
  ])).toHaveLength(2)
 })
})

test("optional undefined binding members are absent; unknown members remain invalid", () => {
 expect(encodeComputerSecrets([{secret:"token",env:{name:"TOKEN",mode:"raw",allowedOrigins:undefined},file:undefined}])).toHaveLength(1)
 expect(encodeComputerSecrets([{secret:"token",file:{path:"/run/secrets/key"},env:undefined}])).toHaveLength(1)
 expect(() => encodeComputerSecrets([{secret:"token",env:{name:"TOKEN",mode:"raw",typo:undefined}} as never])).toThrow("unknown")
})
test("managed env names match server and guest", async () => {
 const vectors = await Bun.file(new URL("../../../internal/computer/testdata/secret-env-names.json",import.meta.url)).json()
 for (const vector of vectors) {
  const encode = () => encodeComputerSecrets([{secret:"token",env:{name:vector.name,mode:"raw"}}])
  if(vector.reserved) expect(encode).toThrow()
  else expect(encode()).toHaveLength(1)
 }
})
test("aggregate origins count normalized entries per binding, including ports", () => {
 for(const variant of ["distinct","repeated","ports","dedup"]) {
  const bindings = Array.from({length:16},(_,i)=>({secret:"token",env:{name:`TOKEN_${i}`,mode:"protected" as const,allowedOrigins:Array.from({length:16},(_,j)=>variant==="dedup"?"https://EXAMPLE.com:443/":variant==="ports"?`https://example.com:${1000+j}`:`https://h${variant==="repeated"?j:i*16+j}.example.com`)}}))
  expect(encodeComputerSecrets(bindings)).toHaveLength(16)
  bindings.push({secret:"token",env:{name:"EXTRA",mode:"protected",allowedOrigins:["https://extra.example.com"]}})
  if(variant==="dedup") expect(encodeComputerSecrets(bindings)).toHaveLength(17)
  else expect(()=>encodeComputerSecrets(bindings)).toThrow("256")
 }
})
