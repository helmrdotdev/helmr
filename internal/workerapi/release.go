package workerapi

// ReleaseObject is one object of a release published for worker hosts.
type ReleaseObject struct {
	Digest    string `json:"digest"`
	SizeBytes int64  `json:"size_bytes"`
}

// Release is the Control Plane's own release as published for worker hosts:
// a host installs or updates to exactly this, never ahead of it.
type Release struct {
	Version   string        `json:"version"`
	Index     ReleaseObject `json:"index"`
	Signature ReleaseObject `json:"signature"`
}

// ReleaseResponse names the Control Plane's release. A Control Plane built
// from source has none, and its hosts never update themselves.
type ReleaseResponse struct {
	Release *Release `json:"release"`
}

// ReleaseReadRequest asks for a read grant of one release object.
type ReleaseReadRequest struct {
	Digest string `json:"digest"`
}
