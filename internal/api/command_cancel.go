package api

type CommandCancelReceipt struct {
	ID       string `json:"id"`
	TargetID string `json:"target_id"`
	Status   string `json:"status"`
}
