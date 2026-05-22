package api

import "time"

type ProjectSummary struct {
	ID           string               `json:"id"`
	Slug         string               `json:"slug"`
	Name         string               `json:"name"`
	IsDefault    bool                 `json:"is_default"`
	CreatedAt    time.Time            `json:"created_at"`
	UpdatedAt    time.Time            `json:"updated_at"`
	Environments []EnvironmentSummary `json:"environments,omitempty"`
}

type EnvironmentSummary struct {
	ID        string    `json:"id"`
	ProjectID string    `json:"project_id"`
	Slug      string    `json:"slug"`
	Name      string    `json:"name"`
	IsDefault bool      `json:"is_default"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type ListProjectsResponse struct {
	Projects []ProjectSummary `json:"projects"`
}

type CreateProjectRequest struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
}

type UpdateProjectRequest struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
}

type CreateEnvironmentRequest struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
}

type UpdateEnvironmentRequest struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
}

type CreateDeploymentRequest struct {
	ProjectID     string                 `json:"project_id,omitempty"`
	EnvironmentID string                 `json:"environment_id,omitempty"`
	Tasks         []DeploymentTaskCreate `json:"tasks,omitempty"`
}

type DeploymentTaskCreate struct {
	TaskID             string `json:"task_id"`
	ModulePath         string `json:"module_path"`
	ExportName         string `json:"export_name"`
	RequestedMilliCPU  int64  `json:"requested_milli_cpu"`
	RequestedMemoryMiB int64  `json:"requested_memory_mib"`
}

type DeploymentResponse struct {
	ID             string                   `json:"id"`
	ProjectID      string                   `json:"project_id"`
	EnvironmentID  string                   `json:"environment_id"`
	SourceArtifact TaskSourceArtifact       `json:"source_artifact"`
	Status         string                   `json:"status"`
	Tasks          []DeploymentTaskResponse `json:"tasks"`
	CreatedAt      time.Time                `json:"created_at"`
	DeployedAt     time.Time                `json:"deployed_at"`
}

type GetCurrentDeploymentResponse struct {
	Deployment *DeploymentResponse `json:"deployment"`
}

const TaskSourceArtifactMediaType = "application/vnd.helmr.task-source.v1.tar"

type TaskSourceArtifact struct {
	Digest    string `json:"digest"`
	SizeBytes int64  `json:"size_bytes,omitempty"`
	MediaType string `json:"media_type,omitempty"`
}

type DeploymentTaskResponse struct {
	ID         string    `json:"id"`
	TaskID     string    `json:"task_id"`
	ModulePath string    `json:"module_path,omitempty"`
	ExportName string    `json:"export_name,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}
