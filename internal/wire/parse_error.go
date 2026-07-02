package wire

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/helmrdotdev/helmr/internal/frameio"
)

const parseErrorFrameType = "parse_error"

type ParseErrorFrame struct {
	Type    string `json:"type"`
	Kind    string `json:"kind"`
	Message string `json:"message"`
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
	return frameio.WriteMessageFrame(w, body)
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
