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
	ID             string   `json:"id"`
	Provider       string   `json:"provider"`
	ProviderRegion string   `json:"provider_region"`
	DisplayName    string   `json:"display_name"`
	State          string   `json:"state"`
	Visibility     string   `json:"visibility"`
	Location       string   `json:"location,omitempty"`
	StaticIPs      []string `json:"static_ips,omitempty"`
}

type ListRegionsResponse struct {
	Regions []RegionSummary `json:"regions"`
}
