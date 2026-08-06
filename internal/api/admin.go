package api

type AdminRegion struct {
	ID             string `json:"id"`
	Provider       string `json:"provider"`
	ProviderRegion string `json:"provider_region"`
	DisplayName    string `json:"display_name"`
	Location       string `json:"location"`
	Visibility     string `json:"visibility"`
	State          string `json:"state"`
}

type AdminRegionsResponse struct {
	Regions []AdminRegion `json:"regions"`
}

type CreateAdminRegionRequest struct {
	ID             string `json:"id"`
	Provider       string `json:"provider"`
	ProviderRegion string `json:"provider_region"`
	DisplayName    string `json:"display_name"`
	Location       string `json:"location"`
	Visibility     string `json:"visibility"`
}

type UpdateAdminRegionRequest struct {
	DisplayName *string `json:"display_name,omitempty"`
	Location    *string `json:"location,omitempty"`
	Visibility  *string `json:"visibility,omitempty"`
}

type AdminWorkerGroup struct {
	ID                              string `json:"id"`
	RegionID                        string `json:"region_id"`
	Name                            string `json:"name"`
	Description                     string `json:"description"`
	State                           string `json:"state"`
	ClaimVersion                    int64  `json:"claim_version"`
	AllowsRun                       bool   `json:"allows_run"`
	AllowsBuild                     bool   `json:"allows_build"`
	RequiredCPUMillis               int64  `json:"required_cpu_millis"`
	RequiredMemoryBytes             int64  `json:"required_memory_bytes"`
	RequiredGuestEphemeralDiskBytes int64  `json:"required_guest_ephemeral_disk_bytes"`
	RequiredBuildCacheBytes         int64  `json:"required_build_cache_bytes"`
	RequiredArtifactCacheBytes      int64  `json:"required_artifact_cache_bytes"`
	RequiredVMSlots                 int32  `json:"required_vm_slots"`
	RequiredBuildExecutors          int32  `json:"required_build_executors"`
	ObservationTTLSeconds           int32  `json:"observation_ttl_seconds"`
}

type AdminWorkerGroupsResponse struct {
	WorkerGroups []AdminWorkerGroup `json:"worker_groups"`
}

type CreateAdminWorkerGroupRequest struct {
	RegionID                        string `json:"region_id"`
	Name                            string `json:"name"`
	Description                     string `json:"description"`
	AllowsRun                       bool   `json:"allows_run"`
	AllowsBuild                     bool   `json:"allows_build"`
	RequiredCPUMillis               int64  `json:"required_cpu_millis"`
	RequiredMemoryBytes             int64  `json:"required_memory_bytes"`
	RequiredGuestEphemeralDiskBytes int64  `json:"required_guest_ephemeral_disk_bytes"`
	RequiredBuildCacheBytes         int64  `json:"required_build_cache_bytes"`
	RequiredArtifactCacheBytes      int64  `json:"required_artifact_cache_bytes"`
	RequiredVMSlots                 int32  `json:"required_vm_slots"`
	RequiredBuildExecutors          int32  `json:"required_build_executors"`
	ObservationTTLSeconds           int32  `json:"observation_ttl_seconds"`
}

type CreateAdminWorkerGroupResponse struct {
	WorkerGroup     AdminWorkerGroup `json:"worker_group"`
	EnrollmentToken string           `json:"enrollment_token"`
}

type UpdateAdminWorkerGroupRequest struct {
	Description string `json:"description"`
}

type WorkerGroupLifecycleRequest struct {
	ExpectedClaimVersion int64 `json:"expected_claim_version"`
}

type RotateWorkerGroupTokenResponse struct {
	EnrollmentToken string `json:"enrollment_token"`
}
