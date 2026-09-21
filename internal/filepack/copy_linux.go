//go:build linux

package filepack

import "os"

// Copy exclusively creates an independent sparse file with the source bytes.
func Copy(source string, dest string) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return err
	}
	output, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	closed := false
	cleanup := true
	defer func() {
		if !closed {
			_ = output.Close()
		}
		if cleanup {
			_ = os.Remove(dest)
		}
	}()
	if err := output.Truncate(info.Size()); err != nil {
		return err
	}
	if err := copySparseFile(input, output, info.Size()); err != nil {
		return err
	}
	if err := output.Close(); err != nil {
		closed = true
		return err
	}
	closed = true
	cleanup = false
	return nil
}

func copySparseFile(input *os.File, output *os.File, logicalSize int64) error {
	offset := int64(0)
	buffer := make([]byte, 4<<20)
	for offset < logicalSize {
		dataStart, dataEnd, nextOffset, sparse, err := nextDataRange(input, offset, logicalSize)
		if err != nil {
			return err
		}
		if !sparse {
			return copySparseRange(input, output, buffer, offset, logicalSize)
		}
		if dataStart < dataEnd {
			if err := copySparseRange(input, output, buffer, dataStart, dataEnd); err != nil {
				return err
			}
		}
		offset = nextOffset
	}
	return nil
}

func copySparseRange(input *os.File, output *os.File, buffer []byte, start int64, end int64) error {
	for offset := start; offset < end; {
		remaining := end - offset
		n := int64(len(buffer))
		if remaining < n {
			n = remaining
		}
		chunk := buffer[:n]
		if err := readFullAt(input, chunk, offset); err != nil {
			return err
		}
		if !allZero(chunk) {
			if _, err := output.WriteAt(chunk, offset); err != nil {
				return err
			}
		}
		offset += n
	}
	return nil
}
