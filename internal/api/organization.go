package api

import "time"

type OrganizationSummary struct {
	ID        string    `json:"id"`
	Slug      string    `json:"slug"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

type CreateOrganizationRequest struct {
	Slug       string `json:"slug"`
	Name       string `json:"name"`
	SetupToken string `json:"setup_token"`
}

type RegionSummary struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Location    string `json:"location,omitempty"`
}

type ListRegionsResponse struct {
	Regions []RegionSummary `json:"regions"`
}
