package guestd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestManagedNodeAgenticWork runs representative agent tool work in a real
// Workspace image: the OCI archive the builder produced from the fixture's SDK
// sandbox declaration is unpacked with guestd's own unpacker, and each declared
// task's handler runs under the platform Node through the managed Program
// launch path with the Runtime's Program flags. The guest task protocol and
// launcher are not exercised here. tests/agentic_work_e2e.sh provides the inputs.
func TestManagedNodeAgenticWork(t *testing.T) {
	archive := os.Getenv("HELMR_GUESTD_AGENTIC_WORKSPACE_IMAGE")
	if archive == "" {
		t.Skip("HELMR_GUESTD_AGENTIC_WORKSPACE_IMAGE is not set")
	}
	file, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	image, err := unpackOCIImage(file, t.TempDir())
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	imageRoot := image.RootfsDir
	user, err := resolveRuntimeUser(imageRoot, image.Config.User)
	if err != nil {
		t.Fatal(err)
	}
	if err := prepareLaunchPath(imageRoot, defaultRuntimeWorkdir, user); err != nil {
		t.Fatal(err)
	}
	cleanup, err := mountImageRuntimeFilesystems(imageRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	flags, err := managedProgramNodeFlags()
	if err != nil {
		t.Fatal(err)
	}

	// launch runs one command in the Workspace; managed selects the Program
	// mounts and sanitized environment, otherwise it is an ordinary Workspace
	// command as `exec` would start it.
	launch := func(t *testing.T, managed bool, path string, args ...string) (string, string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
		defer cancel()
		env := imageRuntimeEnv(image.Config, user, defaultRuntimeWorkdir)
		if managed {
			env = managedRuntimeEnv(image.Config, user, defaultRuntimeWorkdir)
		}
		cmd, err := imageCommand(ctx, path, args, defaultRuntimeWorkdir, env, imageRoot, user,
			imageCommandOptions{ManagedProgram: managed})
		if err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("%s %v: %v (deadline exceeded: %v)\nstdout: %s\nstderr: %s", path, args, err, ctx.Err() != nil, stdout.String(), stderr.String())
		}
		return stdout.String(), stderr.String()
	}
	invoke := func(t *testing.T, task string, output any) {
		t.Helper()
		stdout, stderr := launch(t, true, managedProgramNode,
			append(append([]string{}, flags...), "/opt/helmr/program/probe/invoke.mjs", task)...)
		t.Logf("%s", stdout)
		var result struct {
			Task   string          `json:"task"`
			Output json.RawMessage `json:"output"`
		}
		lines := strings.Split(strings.TrimSpace(stdout), "\n")
		if err := json.Unmarshal([]byte(lines[len(lines)-1]), &result); err != nil || result.Task != task {
			t.Fatalf("task output %q: %v\nstderr: %s", stdout, err, stderr)
		}
		if err := json.Unmarshal(result.Output, output); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("workspace tools run directly", func(t *testing.T) {
		if out, _ := launch(t, false, "/opt/agentic-python/bin/python", "-c",
			"import numpy; print(numpy.__version__, int(numpy.arange(5).sum()))"); strings.TrimSpace(out) != "2.5.3 10" {
			t.Fatalf("python = %q", out)
		}
		if out, _ := launch(t, false, "/usr/bin/git", "--version"); !strings.HasPrefix(out, "git version ") {
			t.Fatalf("git = %q", out)
		}
	})

	t.Run("git edit test diff", func(t *testing.T) {
		var got struct {
			FailedBefore int      `json:"failedBefore"`
			PassedAfter  int      `json:"passedAfter"`
			Diff         string   `json:"diff"`
			Log          []string `json:"log"`
			Clean        bool     `json:"clean"`
		}
		invoke(t, "git-edit", &got)
		if got.FailedBefore == 0 || got.PassedAfter != 0 {
			t.Fatalf("tests before/after edit exited %d/%d", got.FailedBefore, got.PassedAfter)
		}
		if !strings.Contains(got.Diff, "-export const total = values => values.reduce((sum, value) => sum - value, 0)") ||
			!strings.Contains(got.Diff, "+export const total = values => values.reduce((sum, value) => sum + value, 0)") {
			t.Fatalf("diff = %q", got.Diff)
		}
		if !reflect.DeepEqual(got.Log, []string{"fix total", "add total"}) || !got.Clean {
			t.Fatalf("history = %q clean = %v", got.Log, got.Clean)
		}
	})

	t.Run("browser page", func(t *testing.T) {
		var got struct {
			BrowserVersion string              `json:"browserVersion"`
			Heading        string              `json:"heading"`
			Orders         []map[string]string `json:"orders"`
			Result         string              `json:"result"`
			Form           map[string]string   `json:"form"`
			Received       []map[string]string `json:"received"`
			Screenshot     struct {
				Format                  string
				Width, Height, Contrast int
			} `json:"screenshot"`
			WhileOpen  int      `json:"browserProcessesWhileOpen"`
			AfterClose []string `json:"browserProcessesAfterClose"`
		}
		invoke(t, "browser-page", &got)
		if got.BrowserVersion == "" || got.Heading != "Pending orders" ||
			!reflect.DeepEqual(got.Orders, []map[string]string{{"id": "17", "name": "Anvil"}, {"id": "23", "name": "Rope"}}) {
			t.Fatalf("extracted page = %+v", got)
		}
		order := map[string]string{"customer": "Wile E.", "priority": "urgent"}
		if got.Result != "approved for Wile E. (urgent)" || !reflect.DeepEqual(got.Form, order) ||
			!reflect.DeepEqual(got.Received, []map[string]string{order}) {
			t.Fatalf("form interaction = %+v", got)
		}
		if got.Screenshot.Format != "png" || got.Screenshot.Width != 640 || got.Screenshot.Height != 480 || got.Screenshot.Contrast < 10 {
			t.Fatalf("screenshot = %+v", got.Screenshot)
		}
		if got.WhileOpen == 0 || len(got.AfterClose) != 0 {
			t.Fatalf("browser processes: %d while open, %q after close", got.WhileOpen, got.AfterClose)
		}
	})

	t.Run("python native data", func(t *testing.T) {
		var got struct {
			Progress []string `json:"progress"`
			Result   struct {
				NumPy    string    `json:"numpy"`
				Mean     float64   `json:"mean"`
				Std      float64   `json:"std"`
				Solution []float64 `json:"solution"`
			} `json:"result"`
		}
		invoke(t, "python-data", &got)
		if !reflect.DeepEqual(got.Progress, []string{"loaded 10", "solved"}) || got.Result.NumPy != "2.5.3" ||
			got.Result.Mean != 5.5 || got.Result.Std < 2.8722813 || got.Result.Std > 2.8722814 ||
			!reflect.DeepEqual(got.Result.Solution, []float64{2, 3}) {
			t.Fatalf("python result = %+v", got)
		}
	})

	t.Run("sharp image transform", func(t *testing.T) {
		var got struct {
			Format        string
			Width, Height int
			Top, Bottom   []int
		}
		invoke(t, "image-transform", &got)
		if got.Format != "png" || got.Width != 24 || got.Height != 32 ||
			!reflect.DeepEqual(got.Top, []int{255, 0, 0}) || !reflect.DeepEqual(got.Bottom, []int{0, 0, 255}) {
			t.Fatalf("image = %+v", got)
		}
	})

	t.Run("tool process lifecycle", func(t *testing.T) {
		var got struct {
			Exchange struct {
				Code  int
				Reply struct{ Count, Sum int }
			}
			Failure struct {
				Code           int
				Stdout, Stderr string
			}
			Hang struct {
				TimedOut                bool
				Signal                  string
				WhileRunning, Lingering int
				ElapsedMs               int
			}
		}
		invoke(t, "tool-process", &got)
		if got.Exchange.Code != 0 || got.Exchange.Reply.Count != 6 || got.Exchange.Reply.Sum != 108 {
			t.Fatalf("JSON exchange = %+v", got.Exchange)
		}
		if got.Failure.Code != 3 || got.Failure.Stdout != "" || got.Failure.Stderr != "tool: refusing malformed request" {
			t.Fatalf("failure = %+v", got.Failure)
		}
		if !got.Hang.TimedOut || got.Hang.Signal != "SIGKILL" || got.Hang.WhileRunning < 2 || got.Hang.Lingering != 0 ||
			got.Hang.ElapsedMs < 1000 || got.Hang.ElapsedMs > 30000 {
			t.Fatalf("bounded termination = %+v", got.Hang)
		}
	})
}
