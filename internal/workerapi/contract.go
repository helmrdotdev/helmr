package workerapi

// Contract identifies the worker API contract for enrollment, authentication and
// activation. Bump it with incompatible worker API changes; compatible releases
// keep the same revision. Operators drain workers before incompatible upgrades.
const Contract = "helmr.worker-api.v1.r1"

const ContractMismatchCode = "worker_contract_mismatch"
