package api

// WorkerGroup is a worker group the organization operates. Its machines run
// the work its environments' placements send to it.
type WorkerGroup struct {
	ID           string `json:"id"`
	RegionID     string `json:"region_id"`
	Name         string `json:"name"`
	Description  string `json:"description"`
	Status       string `json:"status"`
	ClaimVersion int64  `json:"claim_version"`
}

type WorkerGroupsResponse struct {
	WorkerGroups []WorkerGroup `json:"worker_groups"`
}

type CreateWorkerGroupRequest struct {
	RegionID    string `json:"region_id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// CreateWorkerGroupResponse returns the group's enrollment token once.
type CreateWorkerGroupResponse struct {
	WorkerGroup     WorkerGroup `json:"worker_group"`
	EnrollmentToken string      `json:"enrollment_token"`
}

type FenceWorkerHostRequest struct {
	ExpectedClaimVersion int64 `json:"expected_claim_version"`
}

// Placement maps a placement name in an environment to its capacity. An
// empty WorkerGroupID is the deployment capacity of the environment's region.
type Placement struct {
	Name          string `json:"name"`
	WorkerGroupID string `json:"worker_group_id,omitempty"`
}

type PlacementsResponse struct {
	Placements []Placement `json:"placements"`
}

// SetPlacementRequest assigns a placement to an organization worker group, or
// to deployment capacity when WorkerGroupID is empty.
type SetPlacementRequest struct {
	WorkerGroupID string `json:"worker_group_id"`
}
