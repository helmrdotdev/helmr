package hostconfig

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/helmrdotdev/helmr/internal/frameio"
)

// The evaluator carries its TypeScript loader; nothing is installed beside the
// project and no other file accompanies it.
//
//go:embed config-evaluator.mjs
var evaluator []byte

const (
	minimumNodeMajor  = 22
	maxDocumentBytes  = 1 << 20
	preparationAdvice = "helmr.config.ts is evaluated on this machine before the Linux build: install the packages it imports here " +
		"(normally with your package manager). If the error rejects a config key, check the key's spelling and that the project's @helmr/sdk is compatible with this CLI."
)

// Evaluate runs helmr.config.ts exactly once, as trusted local code in a fresh
// Node process with the caller's environment unchanged. Helmr adds no
// credentials to it. Config output goes to diagnostics; the result arrives on
// its own descriptor.
func Evaluate(ctx context.Context, project string, diagnostics io.Writer) (Document, error) {
	if project == "" || !filepath.IsAbs(project) || filepath.Clean(project) != project {
		return Document{}, errors.New("config project must be an absolute clean path")
	}
	node, err := supportedNode(ctx)
	if err != nil {
		return Document{}, err
	}
	scratch, err := os.MkdirTemp("", "helmr-config-")
	if err != nil {
		return Document{}, err
	}
	defer os.RemoveAll(scratch)
	entry := filepath.Join(scratch, "config-evaluator.mjs")
	if err := os.WriteFile(entry, evaluator, 0o600); err != nil {
		return Document{}, err
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		return Document{}, err
	}
	defer reader.Close()
	command := exec.CommandContext(ctx, node, entry, project)
	command.Dir = project
	command.Stdin = nil
	command.Stdout = diagnostics
	command.Stderr = diagnostics
	command.ExtraFiles = []*os.File{writer}
	if err := command.Start(); err != nil {
		writer.Close()
		return Document{}, fmt.Errorf("start config evaluation: %w", err)
	}
	writer.Close()
	raw, readErr := frameio.ReadMessageFrameBounded(reader, maxDocumentBytes)
	var trailing [1]byte
	if readErr == nil {
		if _, err := io.ReadFull(reader, trailing[:]); !errors.Is(err, io.EOF) {
			readErr = errors.New("config evaluation wrote trailing data")
		}
	}
	_, _ = io.Copy(io.Discard, reader)
	if err := command.Wait(); err != nil {
		return Document{}, fmt.Errorf("evaluate helmr.config.ts: %w\n%s", errors.Join(err, ctx.Err()), preparationAdvice)
	}
	if readErr != nil {
		return Document{}, fmt.Errorf("read resolved config: %w", readErr)
	}
	return parseDocument(raw)
}

func supportedNode(ctx context.Context) (string, error) {
	requirement := fmt.Sprintf("helmr build evaluates helmr.config.ts with Node.js %d or newer from PATH; install it (Helmr does not download Node)", minimumNodeMajor)
	node, err := exec.LookPath("node")
	if err != nil {
		return "", errors.New(requirement)
	}
	output, err := exec.CommandContext(ctx, node, "-p", "process.versions.node").Output()
	if err != nil {
		return "", fmt.Errorf("%s: %w", requirement, err)
	}
	version := strings.TrimSpace(string(output))
	major, err := strconv.Atoi(strings.SplitN(version, ".", 2)[0])
	if err != nil || major < minimumNodeMajor {
		return "", fmt.Errorf("%s; found %s at %s", requirement, version, node)
	}
	return node, nil
}
