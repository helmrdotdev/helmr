package secretproxy

import (
	"io"
	"log"
)

// net/http TLS diagnostics must not include arbitrary guest-controlled values.
func discardLogger() *log.Logger { return log.New(io.Discard, "", 0) }
