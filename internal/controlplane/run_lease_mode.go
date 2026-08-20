package controlplane

import "errors"

var errStaleRunLeaseClaim = errors.New("run lease claim is stale")

type runLeaseClaimMode string

const (
	runLeaseClaimFresh   runLeaseClaimMode = "fresh"
	runLeaseClaimRestore runLeaseClaimMode = "restore"
)
