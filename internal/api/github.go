package api

type GitHubInstallationSummary struct {
	InstallationID      string `json:"installation_id"`
	AccountLogin        string `json:"account_login"`
	AccountType         string `json:"account_type"`
	RepositorySelection string `json:"repository_selection,omitempty"`
	Status              string `json:"status"`
	HTMLURL             string `json:"html_url,omitempty"`
	CreatedAt           string `json:"created_at"`
	UpdatedAt           string `json:"updated_at"`
}

type GitHubRepositorySummary struct {
	GitHubRepositoryID         string                                  `json:"github_repository_id"`
	InstallationID             string                                  `json:"installation_id"`
	FullName                   string                                  `json:"full_name"`
	OwnerLogin                 string                                  `json:"owner_login"`
	Name                       string                                  `json:"name"`
	Private                    bool                                    `json:"private"`
	Archived                   bool                                    `json:"archived"`
	DefaultBranch              string                                  `json:"default_branch,omitempty"`
	Status                     string                                  `json:"status"`
	AccessEnabled              bool                                    `json:"access_enabled"`
	ProjectWorkspaceRepository *GitHubProjectWorkspaceRepositoryStatus `json:"project_workspace_repository,omitempty"`
	HTMLURL                    string                                  `json:"html_url,omitempty"`
	UpdatedAt                  string                                  `json:"updated_at,omitempty"`
}

type GitHubProjectWorkspaceRepositoryStatus struct {
	ProjectID string `json:"project_id"`
	Status    string `json:"status"`
	Enabled   bool   `json:"enabled"`
}

type GitHubInstallationsResponse struct {
	InstallURL    string                      `json:"install_url"`
	Installations []GitHubInstallationSummary `json:"installations"`
}

type GitHubSetupStartRequest struct {
	InstallationID string `json:"installation_id"`
	SetupAction    string `json:"setup_action,omitempty"`
}

type GitHubRepositoryAccessRequest struct {
	InstallationID     string `json:"installation_id"`
	GitHubRepositoryID string `json:"github_repository_id"`
}

type EnableProjectWorkspaceRepositoryRequest struct {
	InstallationID     string `json:"installation_id"`
	GitHubRepositoryID string `json:"github_repository_id"`
	ProjectID          string `json:"project_id,omitempty"`
}

type DisableProjectWorkspaceRepositoryRequest struct {
	InstallationID     string `json:"installation_id"`
	GitHubRepositoryID string `json:"github_repository_id"`
	ProjectID          string `json:"project_id,omitempty"`
}
