package frameio

import (
	"encoding/binary"
	"fmt"
	"io"
)

var streamFrameMagic = [4]byte{'H', 'M', 'S', '2'}

func WriteStreamFrameHeader(w io.Writer, headerBytes []byte, bodyLen uint64) error {
	if len(headerBytes) > MaxFrameBytes {
		return fmt.Errorf("frameio stream frame header length %d exceeds max %d", len(headerBytes), MaxFrameBytes)
	}
	totalLen := uint64(len(headerBytes)) + bodyLen
	var prefix [16]byte
	copy(prefix[:4], streamFrameMagic[:])
	binary.BigEndian.PutUint64(prefix[4:12], totalLen)
	binary.BigEndian.PutUint32(prefix[12:], uint32(len(headerBytes)))
	if _, err := w.Write(prefix[:]); err != nil {
		return err
	}
	_, err := w.Write(headerBytes)
	return err
}

func IsStreamFramePrefix(prefix []byte) bool {
	return len(prefix) >= len(streamFrameMagic) &&
		prefix[0] == streamFrameMagic[0] &&
		prefix[1] == streamFrameMagic[1] &&
		prefix[2] == streamFrameMagic[2] &&
		prefix[3] == streamFrameMagic[3]
}

func ReadStreamFrameHeader(r io.Reader) ([]byte, uint64, error) {
	var prefix [16]byte
	if _, err := io.ReadFull(r, prefix[:]); err != nil {
		return nil, 0, err
	}
	if !IsStreamFramePrefix(prefix[:4]) {
		return nil, 0, fmt.Errorf("frameio stream frame magic mismatch")
	}
	totalLen := binary.BigEndian.Uint64(prefix[4:12])
	headerLen := binary.BigEndian.Uint32(prefix[12:])
	if uint64(headerLen) > totalLen {
		return nil, 0, fmt.Errorf("frameio stream frame header length %d exceeds frame length %d", headerLen, totalLen)
	}
	if headerLen > MaxFrameBytes {
		return nil, 0, fmt.Errorf("frameio stream frame header length %d exceeds max %d", headerLen, MaxFrameBytes)
	}
	headerBytes := make([]byte, headerLen)
	if _, err := io.ReadFull(r, headerBytes); err != nil {
		return nil, 0, err
	}
	return headerBytes, totalLen - uint64(headerLen), nil
}
