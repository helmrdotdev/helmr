// Package filepack encodes sparse runtime files and restores their exact logical bytes.
package filepack

type Stats struct {
	LogicalBytes       int64
	EncodedChunks      int64
	UnpackWrittenBytes int64
}
