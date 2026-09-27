package workerapi

import (
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/computer/blockformat"
)

// ComputerSaveBeginRequest identifies the physical writer and one publication.
// Saves survive individual Run or Command completion.
type ComputerSaveBeginRequest struct {
	EnvironmentID      string `json:"environment_id"`
	ComputerInstanceID string `json:"computer_instance_id"`
	WriterGeneration   int64  `json:"writer_generation"`
	SaveID             string `json:"save_id"`
	Sequence           int64  `json:"sequence"`
}

type ComputerSaveBeginResponse struct {
	ComputerInstanceID string `json:"computer_instance_id"`
	WriterGeneration   int64  `json:"writer_generation"`
	PredecessorID      string `json:"predecessor_id"`
	DesiredVersion     int64  `json:"desired_version"`
	SaveID             string `json:"save_id"`
	Sequence           int64  `json:"sequence"`
}

type ComputerSaveObjectRequest struct {
	Save       ComputerSaveBeginRequest     `json:"save"`
	Inspection blockformat.ObjectInspection `json:"inspection"`
}

type ComputerSavePublicationRequest struct {
	Save ComputerSaveBeginRequest `json:"save"`
	Root computer.GenerationRoot  `json:"root"`
}

type ComputerSavePublicationResponse struct {
	ComputerID string `json:"computer_id"`
	VersionID  string `json:"version_id"`
}
