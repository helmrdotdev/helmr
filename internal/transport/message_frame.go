package transport

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	runv0 "github.com/helmrdotdev/helmr/internal/gen/helmr/run/v0"
	"google.golang.org/protobuf/proto"
)

const MaxFrameBytes = 256 * 1024 * 1024

const parseErrorFrameType = "parse_error"

type ParseErrorFrame struct {
	Type    string `json:"type"`
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

func ReadMessageFrame(r io.Reader) ([]byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}
	size := binary.BigEndian.Uint32(header[:])
	if size > MaxFrameBytes {
		return nil, fmt.Errorf("transport message frame length %d exceeds max %d", size, MaxFrameBytes)
	}
	body := make([]byte, size)
	_, err := io.ReadFull(r, body)
	return body, err
}

func WriteMessageFrame(w io.Writer, body []byte) error {
	if len(body) > MaxFrameBytes {
		return fmt.Errorf("transport message frame length %d exceeds max %d", len(body), MaxFrameBytes)
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(body)))
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	_, err := w.Write(body)
	return err
}

func WriteParseErrorFrame(w io.Writer, kind string, message string) error {
	body, err := json.Marshal(ParseErrorFrame{
		Type:    parseErrorFrameType,
		Kind:    strings.TrimSpace(kind),
		Message: strings.TrimSpace(message),
	})
	if err != nil {
		return fmt.Errorf("marshal parse error frame: %w", err)
	}
	return WriteMessageFrame(w, body)
}

func DecodeParseErrorFrame(body []byte) (ParseErrorFrame, bool, error) {
	var frame ParseErrorFrame
	if err := json.Unmarshal(body, &frame); err != nil {
		return ParseErrorFrame{}, false, nil
	}
	if frame.Type != parseErrorFrameType {
		return ParseErrorFrame{}, false, nil
	}
	frame.Kind = strings.TrimSpace(frame.Kind)
	frame.Message = strings.TrimSpace(frame.Message)
	if frame.Kind == "" {
		return ParseErrorFrame{}, true, errors.New("parse error frame kind is required")
	}
	if frame.Message == "" {
		frame.Message = frame.Kind
	}
	return frame, true, nil
}

func WriteProtoFrame(w io.Writer, message proto.Message) error {
	body, err := proto.Marshal(message)
	if err != nil {
		return fmt.Errorf("marshal transport proto frame: %w", err)
	}
	return WriteMessageFrame(w, body)
}

func ReadProtoFrame(r io.Reader, message proto.Message) error {
	body, err := ReadMessageFrame(r)
	if err != nil {
		return err
	}
	if err := proto.Unmarshal(body, message); err != nil {
		return fmt.Errorf("unmarshal transport proto frame: %w", err)
	}
	return nil
}

func ReadRunEvent(r io.Reader) (*runv0.RunEvent, error) {
	var event runv0.RunEvent
	if err := ReadProtoFrame(r, &event); err != nil {
		return nil, err
	}
	return &event, nil
}
