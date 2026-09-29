package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

func artifactPayloadDigest(ctx context.Context, artifact *Tree) (string, error) {
	return PayloadDigest(ctx, artifact.ordered, artifact.reader.Open)
}

func PayloadDigest(ctx context.Context, entries []Entry, open func(context.Context, string) (io.ReadCloser, error)) (string, error) {
	ordered := append([]Entry(nil), entries...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Path < ordered[j].Path })
	hash := sha256.New()
	_, _ = io.WriteString(hash, "helmr.program-payload.v0\n")
	for _, entry := range ordered {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if entry.Path == "helmr" || (strings.HasPrefix(entry.Path, "helmr/") && entry.Path != "helmr/app" && !strings.HasPrefix(entry.Path, "helmr/app/")) {
			continue
		}
		record := map[string]any{"path": entry.Path, "kind": entry.Kind, "mode": entry.Mode}
		switch entry.Kind {
		case EntryDirectory:
		case EntrySymlink:
			record["linkTarget"] = entry.LinkTarget
		case EntryRegular:
			reader, err := open(ctx, entry.Path)
			if err != nil {
				return "", err
			}
			content := sha256.New()
			size, readErr := io.Copy(content, io.LimitReader(reader, entry.SizeBytes+1))
			closeErr := reader.Close()
			if err := errors.Join(readErr, closeErr); err != nil {
				return "", err
			}
			if size != entry.SizeBytes {
				return "", fmt.Errorf("input %q changed size during digest", entry.Path)
			}
			record["digest"] = sha256sum.FormatDigest(content.Sum(nil))
			record["sizeBytes"] = size
		default:
			return "", fmt.Errorf("invalid input kind at %q", entry.Path)
		}
		raw, err := json.Marshal(record)
		if err != nil {
			return "", err
		}
		canonical, err := jsoncanon.Transform(raw)
		if err != nil {
			return "", err
		}
		_, _ = hash.Write(canonical)
		_, _ = hash.Write([]byte{'\n'})
	}
	return sha256sum.FormatDigest(hash.Sum(nil)), nil
}
