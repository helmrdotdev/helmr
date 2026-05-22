package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/helmrdotdev/helmr/internal/builder"
	bundlev0 "github.com/helmrdotdev/helmr/internal/gen/helmr/bundle/v0"
	"github.com/helmrdotdev/helmr/internal/sourcetar"
	"github.com/helmrdotdev/helmr/internal/transport"
	"github.com/helmrdotdev/helmr/internal/vm"
	"google.golang.org/protobuf/proto"
)

var ErrCompilerRequired = errors.New("task bundle compiler is required")

type CompileRequest struct {
	Source builder.Source
	TaskID string
}

type Compiler interface {
	Compile(context.Context, CompileRequest) (*bundlev0.Bundle, error)
}

type GuestCompiler struct {
	Connector vm.Connector
	TempDir   string
	RunID     string
}

func (p GuestCompiler) Compile(ctx context.Context, request CompileRequest) (*bundlev0.Bundle, error) {
	if p.Connector == nil {
		return nil, errors.New("task bundle compiler guest connector is required")
	}
	source := request.Source
	if strings.TrimSpace(source.ProjectRoot) == "" {
		return nil, errors.New("source project root is required")
	}
	taskID := strings.TrimSpace(request.TaskID)
	if taskID == "" {
		return nil, errors.New("task id is required")
	}
	sourceTar, cleanup, err := sourcetar.CreateTar(source.ProjectRoot, p.TempDir)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	session, err := p.Connector.Connect(ctx)
	if err != nil {
		return nil, fmt.Errorf("connect task compiler guest: %w", err)
	}
	defer session.Close()
	stream := session.Stream()

	runID := strings.TrimSpace(p.RunID)
	if runID == "" {
		runID = "parse"
	}
	if err := writeFileFrame(stream, transport.StreamHeader{Type: transport.StreamTypeParseSource, RunID: runID, TaskID: taskID}, sourceTar.Path); err != nil {
		return nil, fmt.Errorf("write compiler source: %w", err)
	}
	body, err := transport.ReadMessageFrame(stream)
	if err != nil {
		return nil, fmt.Errorf("read parsed task bundle: %w", err)
	}
	return decodeTaskBundleResponse(body)
}

type TaskParseError struct {
	Kind    string
	Message string
}

func (e TaskParseError) Error() string {
	if strings.TrimSpace(e.Message) == "" {
		return "parse task bundle failed"
	}
	return "parse task bundle: " + e.Message
}

func (e TaskParseError) FailureKind() string {
	switch e.Kind {
	case "task_not_found", "duplicate_task_id", "missing_config":
		return e.Kind
	default:
		return "task_parse_failed"
	}
}

func decodeTaskBundleResponse(body []byte) (*bundlev0.Bundle, error) {
	if frame, ok, err := transport.DecodeParseErrorFrame(body); err != nil {
		return nil, fmt.Errorf("read parsed task bundle: %w", err)
	} else if ok {
		return nil, TaskParseError{Kind: frame.Kind, Message: frame.Message}
	}
	return decodeTaskBundle(body)
}

func decodeTaskBundle(body []byte) (*bundlev0.Bundle, error) {
	var bundle bundlev0.Bundle
	if err := proto.Unmarshal(body, &bundle); err != nil {
		return nil, fmt.Errorf("parse task bundle returned invalid bundle protobuf: %w", err)
	}
	if bundle.Image == nil {
		return nil, errors.New("parsed task bundle.image is required")
	}
	return &bundle, nil
}
