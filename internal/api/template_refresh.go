package api

import "time"

type RefreshTemplateRequest struct {
	IdempotencyKey string `json:"idempotency_key"`
}
type TemplateBuildResponse struct {
	DeploymentID string    `json:"deployment_id"`
	TemplateID   string    `json:"template_id"`
	ImageSpecID  string    `json:"image_spec_id"`
	ImageBuildID string    `json:"image_build_id"`
	Status       string    `json:"status"`
	Ready        bool      `json:"ready"`
	Deadline     time.Time `json:"deadline"`
	ErrorCode    string    `json:"error_code,omitempty"`
}
