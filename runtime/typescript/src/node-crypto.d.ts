import "node:crypto"

// Node 24.16+ provides this API; @types/node 24.13.x does not declare it yet.
declare module "node:crypto" {
  function randomUUIDv7(options?: RandomUUIDOptions): UUID
}
