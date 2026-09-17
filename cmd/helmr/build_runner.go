package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"reflect"
	"strings"
)

// dockerBuildRunner retains native Docker auth/TLS settings, but never ambient
// Buildx selection. Its reserved resource is shared across projects and retained.
type dockerBuildRunner struct {
	docker      string
	environment []string
	selector    []string
	contextName string
	endpoint    string
	name        string
}

type buildxNode struct {
	Name     string
	Endpoint string
	Status   string
	Error    string
}
type buildxInstance struct {
	Name   string
	Driver string
	Error  string
	Nodes  []buildxNode
}

func (r dockerBuildRunner) output(ctx context.Context, args ...string) ([]byte, error) {
	process := exec.CommandContext(ctx, r.docker, append(append([]string{}, r.selector...), args...)...)
	process.Env = r.environment
	var stderr bytes.Buffer
	process.Stderr = &stderr
	output, err := process.Output()
	if err != nil {
		return nil, fmt.Errorf("docker %s: %w: %s", strings.Join(args, " "), errors.Join(err, ctx.Err()), strings.TrimSpace(stderr.String()))
	}
	return output, nil
}

func (r dockerBuildRunner) contextEndpoint(ctx context.Context, name string) (string, error) {
	raw, err := r.output(ctx, "context", "inspect", name, "--format", "{{json .Endpoints.docker}}")
	if err != nil {
		return "", err
	}
	var endpoint struct{ Host string }
	if err := json.Unmarshal(raw, &endpoint); err != nil {
		return "", fmt.Errorf("read Docker context endpoint: %w", err)
	}
	if endpoint.Host == "" {
		return "", errors.New("docker context has no Docker endpoint")
	}
	return endpoint.Host, nil
}

func resolveDockerBuildRunner(ctx context.Context) (dockerBuildRunner, error) {
	r := dockerBuildRunner{}
	var err error
	r.docker, err = exec.LookPath("docker")
	if err != nil {
		return r, errors.New("helmr build requires Docker with Buildx; install Docker and start the selected daemon")
	}
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "BUILDX_BUILDER=") && !strings.HasPrefix(entry, "DOCKER_BUILDKIT=") {
			r.environment = append(r.environment, entry)
		}
	}
	r.environment = append(r.environment, "DOCKER_BUILDKIT=1")
	// Explicit context wins over DOCKER_HOST. Otherwise let Docker resolve its
	// native current context (including default when a host override is present).
	r.contextName = os.Getenv("DOCKER_CONTEXT")
	if r.contextName != "" {
		r.selector = []string{"--context", r.contextName}
	} else {
		raw, err := r.output(ctx, "context", "show")
		if err != nil {
			return r, err
		}
		r.contextName = strings.TrimSpace(string(raw))
	}
	if r.contextName == "" {
		return r, errors.New("docker did not select a context")
	}
	r.endpoint, err = r.contextEndpoint(ctx, r.contextName)
	if err != nil {
		return r, err
	}
	if r.contextName == "default" {
		r.selector = []string{"--host", r.endpoint}
	} else {
		r.selector = []string{"--context", r.contextName}
	}
	sum := sha256.Sum256([]byte(r.contextName + "\n" + r.endpoint))
	r.name = fmt.Sprintf("helmr-%x", sum[:12])
	return r, nil
}

func (r dockerBuildRunner) instance(ctx context.Context) (*buildxInstance, error) {
	raw, err := r.output(ctx, "buildx", "ls", "--format", "{{json .}}")
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var found *buildxInstance
	for {
		var item buildxInstance
		if err := decoder.Decode(&item); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("read Buildx listing: %w", err)
		}
		if item.Name != r.name {
			continue
		}
		// Native ls can repeat context entries. Only conflicting selected identities
		// are ambiguous; unrelated/default rows do not establish our identity.
		if found != nil && !reflect.DeepEqual(*found, item) {
			return nil, fmt.Errorf("conflicting Buildx records for %s", r.name)
		}
		found = &item
	}
	if found == nil {
		return nil, nil
	}
	if found.Driver != "docker-container" || len(found.Nodes) != 1 || found.Error != "" {
		return nil, fmt.Errorf("builder %s must have exactly one docker-container node; inspect it with docker buildx inspect %s (Helmr will not replace it)", r.name, r.name)
	}
	node := found.Nodes[0]
	if node.Name == "" || node.Endpoint == "" {
		return nil, fmt.Errorf("builder %s has incomplete node identity", r.name)
	}
	endpoint := node.Endpoint
	if !strings.Contains(endpoint, "://") {
		endpoint, err = r.contextEndpoint(ctx, endpoint)
		if err != nil {
			return nil, err
		}
	}
	if endpoint != r.endpoint || node.Error != "" {
		return nil, fmt.Errorf("builder %s node must use selected Docker endpoint %s; inspect its native configuration", r.name, r.endpoint)
	}
	return found, nil
}

func prepareDockerBuildRunner(ctx context.Context) (dockerBuildRunner, error) {
	r, err := resolveDockerBuildRunner(ctx)
	if err != nil {
		return r, err
	}
	instance, err := r.instance(ctx)
	if err != nil {
		return r, err
	}
	var createErr error
	if instance == nil {
		endpoint := r.contextName
		if endpoint == "default" {
			endpoint = r.endpoint
		}
		_, createErr = r.output(ctx, "buildx", "create", "--name", r.name, "--driver", "docker-container", endpoint)
		if ctx.Err() != nil {
			return r, ctx.Err()
		}
		// One read-back also handles another invocation winning creation. Never
		// append/reconfigure, retry creation, or accept failure without valid identity.
		instance, err = r.instance(ctx)
		if err != nil || instance == nil {
			return r, errors.Join(createErr, err, fmt.Errorf("builder %s was not created with the selected identity", r.name))
		}
	}
	if _, err := r.output(ctx, "buildx", "inspect", "--bootstrap", r.name); err != nil {
		return r, errors.Join(createErr, err)
	}
	instance, err = r.instance(ctx)
	if err != nil {
		return r, errors.Join(createErr, err)
	}
	if instance == nil || instance.Nodes[0].Status != "running" {
		return r, errors.Join(createErr, fmt.Errorf("builder %s is not running after bootstrap; inspect Docker daemon/Buildx diagnostics", r.name))
	}
	return r, nil
}
