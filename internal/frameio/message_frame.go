package frameio

import (
	"encoding/binary"
	"fmt"
	"io"

	"google.golang.org/protobuf/proto"
)

const MaxFrameBytes = 256 * 1024 * 1024

func ReadMessageFrame(r io.Reader) ([]byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}
	size := binary.BigEndian.Uint32(header[:])
	if size > MaxFrameBytes {
		return nil, fmt.Errorf("frameio message frame length %d exceeds max %d", size, MaxFrameBytes)
	}
	body := make([]byte, size)
	_, err := io.ReadFull(r, body)
	return body, err
}

func WriteMessageFrame(w io.Writer, body []byte) error {
	if len(body) > MaxFrameBytes {
		return fmt.Errorf("frameio message frame length %d exceeds max %d", len(body), MaxFrameBytes)
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(body)))
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	_, err := w.Write(body)
	return err
}

func WriteProtoFrame(w io.Writer, message proto.Message) error {
	body, err := proto.Marshal(message)
	if err != nil {
		return fmt.Errorf("marshal frameio proto frame: %w", err)
	}
	return WriteMessageFrame(w, body)
}

func ReadProtoFrame(r io.Reader, message proto.Message) error {
	body, err := ReadMessageFrame(r)
	if err != nil {
		return err
	}
	if err := proto.Unmarshal(body, message); err != nil {
		return fmt.Errorf("unmarshal frameio proto frame: %w", err)
	}
	return nil
}
