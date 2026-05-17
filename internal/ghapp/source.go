package ghapp

import (
	"errors"
	"path"
	"regexp"
	"strings"

	"github.com/helmrdotdev/helmr/internal/api"
)

var repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

type InvalidSourceError struct {
	Err error
}

func (e InvalidSourceError) Error() string {
	return e.Err.Error()
}

func (e InvalidSourceError) Unwrap() error {
	return e.Err
}

func IsInvalidSource(err error) bool {
	var invalid InvalidSourceError
	return errors.As(err, &invalid)
}

func NormalizeSource(source api.GitHubSource) (api.GitHubSource, error) {
	repository := strings.TrimSpace(source.Repository)
	if !repositoryPattern.MatchString(repository) {
		return api.GitHubSource{}, invalidSource(`source.repository must be "owner/repo"`)
	}

	ref := strings.TrimSpace(source.Ref)
	if ref == "" {
		return api.GitHubSource{}, invalidSource("source.ref is required")
	}
	if strings.ContainsRune(ref, '\x00') {
		return api.GitHubSource{}, invalidSource("source.ref contains NUL")
	}
	if source.SHA != "" {
		return api.GitHubSource{}, invalidSource("source.sha is resolved by the server and must not be provided")
	}

	subpath, err := normalizeSubpath(source.Subpath)
	if err != nil {
		return api.GitHubSource{}, err
	}

	return api.GitHubSource{
		Repository: repository,
		Ref:        ref,
		Subpath:    subpath,
	}, nil
}

func normalizeSubpath(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "." {
		return "", nil
	}
	if strings.ContainsRune(raw, '\x00') {
		return "", invalidSource("source.subpath contains NUL")
	}
	if strings.HasPrefix(raw, "/") {
		return "", invalidSource("source.subpath must be relative")
	}
	clean := path.Clean(raw)
	if clean == "." {
		return "", nil
	}
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", invalidSource("source.subpath escapes repository root")
	}
	return clean, nil
}

func invalidSource(message string) error {
	return InvalidSourceError{Err: errors.New(message)}
}
