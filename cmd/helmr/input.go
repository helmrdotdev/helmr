package main

import (
	"encoding/json"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/conversation"
	"github.com/spf13/cobra"
	"os"
	"strings"
)

func parseOptionalJSON(file string, raw string, label string) (json.RawMessage, error) {
	file = strings.TrimSpace(file)
	raw = strings.TrimSpace(raw)
	if file != "" && raw != "" {
		return nil, fmt.Errorf("%s-file cannot be combined with %s-json", label, label)
	}
	if file != "" {
		contents, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("read %s-file: %w", label, err)
		}
		value := json.RawMessage(contents)
		if !json.Valid(value) {
			return nil, fmt.Errorf("%s-file must contain valid JSON", label)
		}
		return value, nil
	}
	if raw == "" {
		return nil, nil
	}
	value := json.RawMessage(raw)
	if !json.Valid(value) {
		return nil, fmt.Errorf("%s-json must be valid JSON", label)
	}
	return value, nil
}

// CLI text entry constructs the same array used by JSON API callers.
func parseContentInput(cmd *cobra.Command, file, raw, label string) (json.RawMessage, error) {
	if cmd.Flags().Changed("text") {
		text, err := cmd.Flags().GetString("text")
		if err != nil {
			return nil, err
		}
		value, err := json.Marshal([]map[string]string{{"type": "text", "text": text}})
		if err != nil {
			return nil, err
		}
		return conversation.Input(value)
	}
	value, err := parseOptionalJSON(file, raw, label)
	if err != nil {
		return nil, err
	}
	if len(value) == 0 {
		return nil, fmt.Errorf("--text, %s-file or %s-json is required", label, label)
	}
	normalized, err := conversation.Input(value)
	if err != nil {
		return nil, fmt.Errorf("input must be an array of text parts: %w", err)
	}
	return normalized, nil
}
