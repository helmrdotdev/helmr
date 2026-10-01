package workerapi

// APIVersion identifies the worker API version for enrollment, authentication and
// activation. Bump it with incompatible worker API changes; compatible releases
// keep the same revision. Operators drain workers before incompatible upgrades.
const APIVersion = "helmr.worker-api.v1.r3"

const APIVersionMismatchCode = "worker_api_version_mismatch"
