package api

type AdminRegion struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Location    string `json:"location"`
}

type AdminRegionsResponse struct {
	Regions []AdminRegion `json:"regions"`
}

type CreateAdminRegionRequest struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Location    string `json:"location"`
}

type UpdateAdminRegionRequest struct {
	DisplayName *string `json:"display_name,omitempty"`
	Location    *string `json:"location,omitempty"`
}

type AdminWorkerGroup struct {
	ID            string `json:"id"`
	RegionID      string `json:"region_id"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	State         string `json:"state"`
	ClaimVersion  int64  `json:"claim_version"`
	PrimaryPoolID string `json:"primary_pool_id,omitempty"`
}

type AdminWorkerGroupsResponse struct {
	WorkerGroups []AdminWorkerGroup `json:"worker_groups"`
}

type CreateAdminWorkerGroupRequest struct {
	RegionID    string `json:"region_id"`
	Name        string `json:"name"`
	Description string `json:"description"`
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

type AdminWorkerPool struct {
	ID            string `json:"id"`
	WorkerGroupID string `json:"worker_group_id"`
	Name          string `json:"name"`
	State         string `json:"state"`
	ClaimVersion  int64  `json:"claim_version"`
	Primary       bool   `json:"primary"`
}

type AdminWorkerPoolsResponse struct {
	WorkerPools []AdminWorkerPool `json:"worker_pools"`
}

type CreateAdminWorkerPoolRequest struct {
	Name                      string `json:"name"`
	ExpectedGroupClaimVersion int64  `json:"expected_group_claim_version"`
}

type SwitchAdminWorkerPoolPrimaryRequest struct {
	ExpectedGroupClaimVersion int64 `json:"expected_group_claim_version"`
}

type SwitchAdminWorkerPoolPrimaryResponse struct {
	WorkerGroup AdminWorkerGroup `json:"worker_group"`
	WorkerPool  AdminWorkerPool  `json:"worker_pool"`
}

type WorkerPoolLifecycleRequest struct {
	ExpectedPoolClaimVersion int64 `json:"expected_pool_claim_version"`
}
