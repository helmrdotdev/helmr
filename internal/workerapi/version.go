package workerapi

// APIVersion identifies the worker API version that workers present at
// enrollment, authentication and activation; any other value is rejected.
const APIVersion = "helmr.worker-api.v1.r1"

const APIVersionMismatchCode = "worker_api_version_mismatch"
