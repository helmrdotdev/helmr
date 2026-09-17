package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// Use a child executable at the actual Docker boundary, including argument and
// environment recording. No production command hook is needed for preparation.
func fakeDocker(t *testing.T, scenario string) string {
	t.Helper()
	root := t.TempDir()
	script, err := filepath.Abs("testdata/docker-buildx")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(script, filepath.Join(root, "docker")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HELMR_TEST_DOCKER", root)
	t.Setenv("HELMR_TEST_SCENARIO", scenario)
	t.Setenv("DOCKER_CONTEXT", "")
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("BUILDX_BUILDER", "unrelated-user-builder")
	return root
}

func dockerCalls(t *testing.T, root string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "calls"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestPrepareDockerBuildRunner(t *testing.T) {
	for _, scenario := range []string{"absent", "running", "stopped", "race", "wrong-driver", "wrong-endpoint", "multi-node", "ls-error", "create-error", "race-wrong", "bootstrap-error", "race-bootstrap", "not-ready", "malformed"} {
		t.Run(scenario, func(t *testing.T) {
			root := fakeDocker(t, scenario)
			r, err := prepareDockerBuildRunner(t.Context())
			success := scenario == "absent" || scenario == "running" || scenario == "stopped" || scenario == "race"
			if (err == nil) != success {
				t.Fatalf("prepare: %+v %v", r, err)
			}
			calls := dockerCalls(t, root)
			if strings.Contains(calls, "--use") || strings.Contains(calls, "--append") || strings.Contains(calls, "buildx rm") || strings.Contains(calls, "buildx stop") {
				t.Fatalf("mutated selection/existing resource: %s", calls)
			}
			for _, bad := range []string{"wrong-driver", "wrong-endpoint", "multi-node", "ls-error", "create-error", "race-wrong", "malformed"} {
				if scenario == bad && strings.Contains(calls, "--bootstrap") {
					t.Fatalf("bootstrap before identity: %s", calls)
				}
			}
			if success {
				if strings.Count(calls, "--bootstrap") != 1 {
					t.Fatal(calls)
				}
				if strings.Contains(calls, "ambient=unrelated") {
					t.Fatal("ambient builder leaked")
				}
			}
			if scenario == "race-bootstrap" && (!strings.Contains(err.Error(), "another invocation") || !strings.Contains(err.Error(), "bootstrap unavailable")) {
				t.Fatal(err)
			}
			if scenario == "create-error" && !strings.Contains(err.Error(), "create denied") {
				t.Fatal(err)
			}
		})
	}
}

func TestDockerSelectionFrozenForEveryBuildStage(t *testing.T) {
	for _, settings := range []struct{ context, host, want string }{{"", "", "--context desktop"}, {"alternate", "", "--context alternate"}, {"", "unix:///host.sock", "--host unix:///host.sock"}, {"alternate", "unix:///ignored.sock", "--context alternate"}} {
		t.Run(settings.want+settings.host, func(t *testing.T) {
			root := fakeDocker(t, "running")
			t.Setenv("DOCKER_CONTEXT", settings.context)
			t.Setenv("DOCKER_HOST", settings.host)
			r, err := prepareDockerBuildRunner(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("DOCKER_CONTEXT", "changed-after-preparation")
			t.Setenv("DOCKER_HOST", "unix:///changed.sock")
			for _, stage := range []string{"installed-tree", "analysis", "workspace", "bundle"} {
				if err := executeDockerBuildx(t.Context(), &cobra.Command{}, dockerBuildxRequest{Runner: r, Target: stage, OutputType: "oci", OutputAttributes: map[string]string{"rewrite-timestamp": "true"}}); err != nil {
					t.Fatal(err)
				}
			}
			calls := dockerCalls(t, root)
			if strings.Contains(calls, "changed-after") || strings.Contains(calls, "changed.sock") {
				t.Fatal(calls)
			}
			for _, stage := range []string{"installed-tree", "analysis", "workspace", "bundle"} {
				var matched bool
				for _, line := range strings.Split(calls, "\n") {
					if strings.Contains(line, "--target "+stage+" ") {
						matched = true
						if !strings.Contains(line, settings.want+" buildx build --builder "+r.name+" ") || !strings.Contains(line, "rewrite-timestamp=true") || !strings.Contains(line, "SOURCE_DATE_EPOCH=0") {
							t.Fatal(line)
						}
					}
				}
				if !matched {
					t.Fatalf("missing %s: %s", stage, calls)
				}
			}
		})
	}
}

func TestDockerCancellationStopsNativeChild(t *testing.T) {
	for _, scenario := range []string{"cancel-create", "cancel-build"} {
		t.Run(scenario, func(t *testing.T) {
			root := fakeDocker(t, scenario)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			result := make(chan error, 1)
			go func() {
				runner, err := prepareDockerBuildRunner(ctx)
				if err == nil && scenario == "cancel-build" {
					err = executeDockerBuildx(ctx, &cobra.Command{}, dockerBuildxRequest{Runner: runner, Target: "installed-tree", OutputType: "oci"})
				}
				result <- err
			}()
			// The fake publishes its entry into the blocked native child. No timed retry
			// of preparation; this loop only observes the test's synchronization file.
			ticker := time.NewTicker(time.Millisecond)
			defer ticker.Stop()
			timeout := time.NewTimer(10 * time.Second)
			defer timeout.Stop()
			for {
				if _, err := os.Stat(filepath.Join(root, "blocked")); err == nil {
					break
				}
				select {
				case <-ticker.C:
				case <-timeout.C:
					cancel()
					<-result
					t.Fatal("child never reached cancellation barrier")
				}
			}
			cancel()
			err := <-result
			if !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			calls := dockerCalls(t, root)
			if scenario == "cancel-create" && (strings.Count(calls, "buildx ls") != 1 || strings.Contains(calls, "--bootstrap")) {
				t.Fatal(calls)
			}

		})
	}
}

func TestDockerMissingIsActionable(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := prepareDockerBuildRunner(t.Context())
	if err == nil || !strings.Contains(err.Error(), "requires Docker with Buildx") {
		t.Fatal(err)
	}
}
