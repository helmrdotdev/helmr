//go:build linux

package guestd

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/archive"
	"github.com/helmrdotdev/helmr/internal/workspace"
)

const workspaceRestoreSpikeProcess = "HELMR_WORKSPACE_RESTORE_SPIKE_PROCESS"

var errWorkspaceRestoreSpikeInterrupted = errors.New("workspace restore materialization interrupted")

type workspaceRestoreSpikeJournal struct {
	Version    int                    `json:"version"`
	Phase      string                 `json:"phase"`
	TargetTree workspace.TreeIdentity `json:"target_tree"`
}

type workspaceRestoreSpikeProbe struct {
	CWDInode           uint64 `json:"cwd_inode"`
	CWD                string `json:"cwd,omitempty"`
	CWDError           string `json:"cwd_error,omitempty"`
	FreshValue         string `json:"fresh_value,omitempty"`
	FreshError         string `json:"fresh_error,omitempty"`
	AddedValue         string `json:"added_value,omitempty"`
	AddedError         string `json:"added_error,omitempty"`
	OpenFileValue      string `json:"open_file_value,omitempty"`
	OpenFileError      string `json:"open_file_error,omitempty"`
	RemovedPathMissing bool   `json:"removed_path_missing,omitempty"`
	OpenDirectoryAlive bool   `json:"open_directory_alive,omitempty"`
	OpenDirectoryInode uint64 `json:"open_directory_inode,omitempty"`
}

func TestWorkspaceRestoreMaterializationSpikePreservesRootCWDAndReplays(t *testing.T) {
	if os.Getenv(workspaceRestoreSpikeProcess) != "" {
		runWorkspaceRestoreSpikeProcess()
		return
	}

	liveRoot, artifact, targetTree := prepareWorkspaceRestoreSpike(t)
	process, input, output := startWorkspaceRestoreSpikeProcess(t, liveRoot, "root")
	ready := readWorkspaceRestoreSpikeProbe(t, output, process)
	rootInode := workspaceRestoreSpikeInode(t, liveRoot)
	if ready.CWDInode != rootInode {
		t.Fatalf("parent cwd inode = %d, workspace root inode = %d", ready.CWDInode, rootInode)
	}
	stopWorkspaceRestoreSpikeProcess(t, process)

	stateRoot := t.TempDir()
	operation := workspaceRestoreSpikeOperation{
		liveRoot:     liveRoot,
		stateRoot:    stateRoot,
		artifactPath: artifact.Path,
		targetTree:   targetTree,
	}
	if err := operation.apply(true); !errors.Is(err, errWorkspaceRestoreSpikeInterrupted) {
		t.Fatalf("interrupted materialization error = %v", err)
	}
	if workspaceRestoreSpikeInode(t, liveRoot) != rootInode {
		t.Fatal("interrupted materialization replaced the Workspace root inode")
	}
	if err := operation.apply(false); err != nil {
		t.Fatalf("replay materialization: %v", err)
	}
	if err := operation.apply(false); err != nil {
		t.Fatalf("idempotent materialization replay: %v", err)
	}
	if workspaceRestoreSpikeInode(t, liveRoot) != rootInode {
		t.Fatal("materialization replaced the Workspace root inode")
	}
	if tree, err := workspace.InspectTree(liveRoot); err != nil || tree != targetTree {
		t.Fatalf("materialized tree = %+v, %v; want %+v", tree, err, targetTree)
	}

	resumeWorkspaceRestoreSpikeProcess(t, process)
	if _, err := io.WriteString(input, "resume\n"); err != nil {
		t.Fatal(err)
	}
	if err := input.Close(); err != nil {
		t.Fatal(err)
	}
	result := readWorkspaceRestoreSpikeProbe(t, output, process)
	waitWorkspaceRestoreSpikeProcess(t, process)
	if result.CWDInode != rootInode || result.CWDError != "" || result.CWD != liveRoot {
		t.Fatalf("resumed root cwd = %+v, want inode=%d path=%q", result, rootInode, liveRoot)
	}
	if result.FreshValue != "C" || result.AddedValue != "added" || !result.RemovedPathMissing {
		t.Fatalf("resumed fresh path observations = %+v", result)
	}
	if result.OpenFileValue != "A" || !result.OpenDirectoryAlive ||
		result.OpenDirectoryInode != ready.OpenDirectoryInode {
		t.Fatalf("resumed open descriptor observations = %+v", result)
	}
}

func TestWorkspaceRestoreMaterializationSpikeDocumentsRenamedNestedCWD(t *testing.T) {
	if os.Getenv(workspaceRestoreSpikeProcess) != "" {
		runWorkspaceRestoreSpikeProcess()
		return
	}

	liveRoot, artifact, targetTree := prepareWorkspaceRestoreSpike(t)
	nestedRoot := filepath.Join(liveRoot, "renamed-from")
	process, input, output := startWorkspaceRestoreSpikeProcess(t, nestedRoot, "nested")
	ready := readWorkspaceRestoreSpikeProbe(t, output, process)
	stopWorkspaceRestoreSpikeProcess(t, process)

	operation := workspaceRestoreSpikeOperation{
		liveRoot:     liveRoot,
		stateRoot:    t.TempDir(),
		artifactPath: artifact.Path,
		targetTree:   targetTree,
	}
	if err := operation.apply(false); err != nil {
		t.Fatal(err)
	}
	resumeWorkspaceRestoreSpikeProcess(t, process)
	if _, err := io.WriteString(input, "resume\n"); err != nil {
		t.Fatal(err)
	}
	if err := input.Close(); err != nil {
		t.Fatal(err)
	}
	result := readWorkspaceRestoreSpikeProbe(t, output, process)
	waitWorkspaceRestoreSpikeProcess(t, process)

	if result.CWDInode != ready.CWDInode || !result.OpenDirectoryAlive ||
		result.OpenDirectoryInode != ready.OpenDirectoryInode || result.OpenFileValue != "old-directory" {
		t.Fatalf("removed nested cwd lost its open inode state = ready=%+v result=%+v", ready, result)
	}
	if result.CWDError == "" || result.FreshError == "" {
		t.Fatalf("removed nested cwd unexpectedly followed the child rename = %+v", result)
	}
	if result.AddedValue != "new-directory" {
		t.Fatalf("fresh lookup of renamed target = %+v", result)
	}
}

type workspaceRestoreSpikeOperation struct {
	liveRoot     string
	stateRoot    string
	artifactPath string
	targetTree   workspace.TreeIdentity
}

func (operation workspaceRestoreSpikeOperation) apply(interrupt bool) error {
	journal, found, err := readWorkspaceRestoreSpikeJournal(operation.stateRoot)
	if err != nil {
		return err
	}
	stagingRoot := filepath.Join(operation.stateRoot, "target")
	if !found {
		if err := os.RemoveAll(stagingRoot); err != nil {
			return err
		}
		if err := os.Mkdir(stagingRoot, 0o700); err != nil {
			return err
		}
		if err := extractWorkspaceRestoreSpikeArtifact(operation.artifactPath, stagingRoot, false); err != nil {
			return fmt.Errorf("prepare target tree: %w", err)
		}
		if tree, err := workspace.InspectTree(stagingRoot); err != nil || tree != operation.targetTree {
			return fmt.Errorf("prepared target tree = %+v, %v; want %+v", tree, err, operation.targetTree)
		}
		if err := syncWorkspaceTree(stagingRoot); err != nil {
			return err
		}
		journal = workspaceRestoreSpikeJournal{Version: 1, Phase: "prepared", TargetTree: operation.targetTree}
		if err := writeWorkspaceRestoreSpikeJournal(operation.stateRoot, journal); err != nil {
			return err
		}
	} else if journal.Version != 1 || journal.TargetTree != operation.targetTree {
		return errors.New("workspace restore spike journal conflicts with its target")
	}

	switch journal.Phase {
	case "applied":
		tree, err := workspace.InspectTree(operation.liveRoot)
		if err != nil || tree != operation.targetTree {
			return fmt.Errorf("replayed live tree = %+v, %v; want %+v", tree, err, operation.targetTree)
		}
		return nil
	case "prepared":
	default:
		return errors.New("workspace restore spike journal phase is invalid")
	}

	if err := pruneWorkspaceRestoreSpikeTree(operation.liveRoot, stagingRoot); err != nil {
		return err
	}
	if err := extractWorkspaceRestoreSpikeArtifact(operation.artifactPath, operation.liveRoot, interrupt); err != nil {
		return err
	}
	tree, err := workspace.InspectTree(operation.liveRoot)
	if err != nil || tree != operation.targetTree {
		return fmt.Errorf("materialized live tree = %+v, %v; want %+v", tree, err, operation.targetTree)
	}
	if err := syncWorkspaceTree(operation.liveRoot); err != nil {
		return err
	}
	journal.Phase = "applied"
	return writeWorkspaceRestoreSpikeJournal(operation.stateRoot, journal)
}

func extractWorkspaceRestoreSpikeArtifact(path, destination string, interrupt bool) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	reader := io.Reader(file)
	if interrupt {
		info, err := file.Stat()
		if err != nil {
			return err
		}
		reader = &workspaceRestoreSpikeInterruptReader{reader: file, remaining: max(1, info.Size()/2)}
	}
	_, err = archive.ExtractTarWithStats(reader, destination, archive.ExtractOptions{
		MaxBytes:   workspace.MaxArtifactExtractedBytes,
		MaxEntries: workspace.MaxArtifactEntries,
	})
	return err
}

type workspaceRestoreSpikeInterruptReader struct {
	reader    io.Reader
	remaining int64
}

func (reader *workspaceRestoreSpikeInterruptReader) Read(buffer []byte) (int, error) {
	if reader.remaining <= 0 {
		return 0, errWorkspaceRestoreSpikeInterrupted
	}
	if int64(len(buffer)) > reader.remaining {
		buffer = buffer[:reader.remaining]
	}
	count, err := reader.reader.Read(buffer)
	reader.remaining -= int64(count)
	return count, err
}

func pruneWorkspaceRestoreSpikeTree(liveRoot, targetRoot string) error {
	targetKinds, err := workspaceRestoreSpikeTreeKinds(targetRoot)
	if err != nil {
		return err
	}
	var livePaths []string
	err = filepath.WalkDir(liveRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path != liveRoot {
			livePaths = append(livePaths, path)
		}
		return nil
	})
	if err != nil {
		return err
	}
	sort.Slice(livePaths, func(left, right int) bool {
		leftDepth := strings.Count(livePaths[left], string(filepath.Separator))
		rightDepth := strings.Count(livePaths[right], string(filepath.Separator))
		if leftDepth != rightDepth {
			return leftDepth > rightDepth
		}
		return livePaths[left] > livePaths[right]
	})
	for _, path := range livePaths {
		rel, err := filepath.Rel(liveRoot, path)
		if err != nil {
			return err
		}
		liveInfo, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		targetKind, exists := targetKinds[rel]
		if exists && targetKind == workspaceRestoreSpikeFileKind(liveInfo.Mode()) {
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			return err
		}
	}
	return syncDirectory(liveRoot)
}

func workspaceRestoreSpikeTreeKinds(root string) (map[string]uint32, error) {
	kinds := make(map[string]uint32)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		kinds[rel] = workspaceRestoreSpikeFileKind(info.Mode())
		return nil
	})
	return kinds, err
}

func workspaceRestoreSpikeFileKind(mode fs.FileMode) uint32 {
	switch {
	case mode.IsDir():
		return 1
	case mode.IsRegular():
		return 2
	case mode&os.ModeSymlink != 0:
		return 3
	default:
		return 0
	}
}

func readWorkspaceRestoreSpikeJournal(root string) (workspaceRestoreSpikeJournal, bool, error) {
	body, err := os.ReadFile(filepath.Join(root, "journal.json"))
	if errors.Is(err, os.ErrNotExist) {
		return workspaceRestoreSpikeJournal{}, false, nil
	}
	if err != nil {
		return workspaceRestoreSpikeJournal{}, false, err
	}
	var journal workspaceRestoreSpikeJournal
	if err := json.Unmarshal(body, &journal); err != nil {
		return workspaceRestoreSpikeJournal{}, false, err
	}
	return journal, true, nil
}

func writeWorkspaceRestoreSpikeJournal(root string, journal workspaceRestoreSpikeJournal) error {
	body, err := json.Marshal(journal)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(root, ".journal-*")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(body); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporary, filepath.Join(root, "journal.json")); err != nil {
		return err
	}
	return syncDirectory(root)
}

func prepareWorkspaceRestoreSpike(t *testing.T) (string, workspace.WorkspaceArtifact, workspace.TreeIdentity) {
	t.Helper()
	tempRoot := t.TempDir()
	liveRoot := filepath.Join(tempRoot, "live")
	targetRoot := filepath.Join(tempRoot, "target")
	for _, root := range []string{liveRoot, targetRoot} {
		if err := os.Mkdir(root, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeWorkspaceRestoreSpikeFile(t, liveRoot, "current.txt", "A")
	writeWorkspaceRestoreSpikeFile(t, liveRoot, "removed.txt", "remove")
	writeWorkspaceRestoreSpikeFile(t, liveRoot, "renamed-from/state.txt", "old-directory")
	writeWorkspaceRestoreSpikeFile(t, liveRoot, "type-change/old.txt", "old-type")
	writeWorkspaceRestoreSpikeFile(t, targetRoot, "current.txt", "C")
	writeWorkspaceRestoreSpikeFile(t, targetRoot, "added.txt", "added")
	writeWorkspaceRestoreSpikeFile(t, targetRoot, "renamed-to/state.txt", "new-directory")
	writeWorkspaceRestoreSpikeFile(t, targetRoot, "type-change", "new-type")
	targetTree, err := workspace.InspectTree(targetRoot)
	if err != nil {
		t.Fatal(err)
	}
	artifact, cleanup, err := workspace.CreateWorkspaceArtifactFromRoot(targetRoot, tempRoot, tempRoot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	return liveRoot, artifact, targetTree
}

func writeWorkspaceRestoreSpikeFile(t *testing.T, root, relative, value string) {
	t.Helper()
	path := filepath.Join(root, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}

func startWorkspaceRestoreSpikeProcess(t *testing.T, cwd, mode string) (*exec.Cmd, io.WriteCloser, *json.Decoder) {
	t.Helper()
	command := exec.Command(os.Args[0], "-test.run=^TestWorkspaceRestoreMaterializationSpikeProcess$")
	command.Dir = cwd
	command.Env = append(os.Environ(), workspaceRestoreSpikeProcess+"="+mode)
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = command.Process.Signal(syscall.SIGCONT)
		_ = command.Process.Kill()
		_, _ = io.Copy(io.Discard, output)
		_ = command.Wait()
		if t.Failed() && stderr.Len() != 0 {
			t.Logf("workspace restore spike process stderr: %s", stderr.String())
		}
	})
	return command, input, json.NewDecoder(output)
}

func stopWorkspaceRestoreSpikeProcess(t *testing.T, command *exec.Cmd) {
	t.Helper()
	if err := command.Process.Signal(syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		body, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", command.Process.Pid))
		if err == nil && bytes.Contains(body, []byte("\nState:\tT")) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("workspace restore spike process did not stop")
}

func resumeWorkspaceRestoreSpikeProcess(t *testing.T, command *exec.Cmd) {
	t.Helper()
	if err := command.Process.Signal(syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
}

func waitWorkspaceRestoreSpikeProcess(t *testing.T, command *exec.Cmd) {
	t.Helper()
	if err := command.Wait(); err != nil {
		t.Fatal(err)
	}
}

func readWorkspaceRestoreSpikeProbe(t *testing.T, decoder *json.Decoder, command *exec.Cmd) workspaceRestoreSpikeProbe {
	t.Helper()
	var probe workspaceRestoreSpikeProbe
	if err := decoder.Decode(&probe); err != nil {
		t.Fatalf("read workspace restore spike process %d: %v", command.Process.Pid, err)
	}
	return probe
}

func workspaceRestoreSpikeInode(t *testing.T, path string) uint64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("workspace restore spike inode is unavailable")
	}
	return stat.Ino
}

func TestWorkspaceRestoreMaterializationSpikeProcess(t *testing.T) {
	mode := os.Getenv(workspaceRestoreSpikeProcess)
	if mode == "" {
		t.Skip("workspace restore spike process")
	}
	runWorkspaceRestoreSpikeProcess()
}

func runWorkspaceRestoreSpikeProcess() {
	mode := os.Getenv(workspaceRestoreSpikeProcess)
	openPath := "current.txt"
	freshPath := "current.txt"
	addedPath := "added.txt"
	removedPath := "removed.txt"
	openDirectoryPath := "renamed-from"
	if mode == "nested" {
		openPath = "state.txt"
		freshPath = "state.txt"
		addedPath = "../renamed-to/state.txt"
		removedPath = "../renamed-from"
		openDirectoryPath = "."
	}
	openFile, err := os.Open(openPath)
	if err != nil {
		panic(err)
	}
	defer openFile.Close()
	openDirectory, err := os.Open(openDirectoryPath)
	if err != nil {
		panic(err)
	}
	defer openDirectory.Close()
	encoder := json.NewEncoder(os.Stdout)
	openDirectoryInfo, err := openDirectory.Stat()
	if err != nil {
		panic(err)
	}
	if err := encoder.Encode(workspaceRestoreSpikeProbe{
		CWDInode:           workspaceRestoreSpikeCurrentInode(),
		OpenDirectoryAlive: true,
		OpenDirectoryInode: workspaceRestoreSpikeStatInode(openDirectoryInfo),
	}); err != nil {
		panic(err)
	}
	if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
		panic(err)
	}
	probe := workspaceRestoreSpikeProbe{CWDInode: workspaceRestoreSpikeCurrentInode()}
	probe.CWD, err = os.Getwd()
	probe.CWDError = workspaceRestoreSpikeError(err)
	probe.FreshValue, probe.FreshError = workspaceRestoreSpikeRead(freshPath)
	probe.AddedValue, probe.AddedError = workspaceRestoreSpikeRead(addedPath)
	_, err = os.Stat(removedPath)
	probe.RemovedPathMissing = errors.Is(err, os.ErrNotExist)
	if _, err := openFile.Seek(0, io.SeekStart); err != nil {
		probe.OpenFileError = err.Error()
	} else {
		body, readErr := io.ReadAll(openFile)
		probe.OpenFileValue = string(body)
		probe.OpenFileError = workspaceRestoreSpikeError(readErr)
	}
	openDirectoryInfo, err = openDirectory.Stat()
	probe.OpenDirectoryAlive = err == nil
	if err == nil {
		probe.OpenDirectoryInode = workspaceRestoreSpikeStatInode(openDirectoryInfo)
	}
	if err := encoder.Encode(probe); err != nil {
		panic(err)
	}
	os.Exit(0)
}

func workspaceRestoreSpikeCurrentInode() uint64 {
	info, err := os.Stat(".")
	if err != nil {
		panic(err)
	}
	return workspaceRestoreSpikeStatInode(info)
}

func workspaceRestoreSpikeStatInode(info fs.FileInfo) uint64 {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		panic("workspace restore spike inode is unavailable")
	}
	return stat.Ino
}

func workspaceRestoreSpikeRead(path string) (string, string) {
	body, err := os.ReadFile(path)
	return string(body), workspaceRestoreSpikeError(err)
}

func workspaceRestoreSpikeError(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
