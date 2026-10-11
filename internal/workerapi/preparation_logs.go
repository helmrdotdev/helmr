package workerapi

type PreparationLogRequest struct {
	Executor           PreparationExecutor `json:"executor"`
	Stream             string              `json:"stream"`
	Kind               string              `json:"kind"`
	Sequence           int64               `json:"sequence"`
	ThroughSequence    int64               `json:"through_sequence"`
	ObservedAtUnixNano int64               `json:"observed_at_unix_nano"`
	Data               []byte              `json:"data"`
	DroppedBytes       int64               `json:"dropped_bytes"`
	Complete           bool                `json:"complete"`
}
