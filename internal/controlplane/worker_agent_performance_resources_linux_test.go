//go:build linux && computerproof

package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"uuid"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/helmrdotdev/helmr/internal/firecracker/custody"
	"github.com/jackc/pgx/v5/pgxpool"
)

// These are sampled observations, not asserted physical maxima. This observer's
// own listing requests and CPU time are recorded separately from instrumented
// Worker/Control Plane requests. It never modifies host caches or remote objects.
func observeNativePerformanceResources(ctx context.Context, input nativeExecutionConfig, pool *pgxpool.Pool, env uuid.UUID, processIDs map[string]int) error {
	store, bucket, prefix, err := nativePerformanceStore(ctx, input)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(filepath.Join(input.Evidence, "performance-resources.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	encoder := json.NewEncoder(file)
	started := time.Now()
	for {
		at := time.Now()
		sample := map[string]any{"at": at.UTC(), "elapsedMS": float64(at.Sub(started)) / float64(time.Millisecond), "requestedIntervalMS": 2000}
		failures := []string{}
		read := func(path string) string {
			data, err := os.ReadFile(path)
			if err != nil {
				failures = append(failures, fmt.Sprintf("%s: %v", path, err))
			}
			return string(data)
		}
		group := filepath.Dir(input.WorkerCgroup)
		cgroup := map[string]string{}
		for _, name := range []string{"cpu.stat", "memory.current", "memory.peak"} {
			cgroup[name] = read(filepath.Join(group, name))
		}
		if data, err := os.ReadFile(filepath.Join(group, "io.stat")); err == nil {
			cgroup["io.stat"] = string(data)
		} else {
			sample["unavailable"] = []string{"worker cgroup I/O counters: " + err.Error()}
		}
		sample["workerCgroup"] = cgroup
		vms, err := nativeVMResources(input.WorkerEnv["WORKER_WORK_DIR"], "/sys/fs/cgroup/firecracker")
		if err != nil {
			failures = append(failures, "VM resource inventory: "+err.Error())
		}
		sample["vmCgroups"] = vms
		processes := map[string]any{}
		for name, pid := range processIDs {
			processes[name] = map[string]any{"pid": pid, "stat": read(fmt.Sprintf("/proc/%d/stat", pid)), "io": read(fmt.Sprintf("/proc/%d/io", pid))}
		}
		sample["processes"] = processes
		sample["hostCPU"] = read("/proc/stat")
		sample["hostMemory"] = read("/proc/meminfo")
		sample["hostNetwork"] = read("/proc/net/dev")
		local, err := nativeLocalFootprint(input.WorkerEnv["WORKER_WORK_DIR"])
		if err != nil {
			failures = append(failures, "local inventory: "+err.Error())
		}
		sample["local"] = local
		observation, cancel := context.WithTimeout(ctx, 10*time.Second)
		var retained json.RawMessage
		err = pool.QueryRow(observation, `SELECT jsonb_build_object('objects',count(*),'bytes',COALESCE(sum(o.size_bytes),0))
 FROM computer_objects o WHERE o.environment_id=$1 AND EXISTS(SELECT 1 FROM computer_object_pins p WHERE p.environment_id=o.environment_id AND p.digest=o.digest)`, env).Scan(&retained)
		if err != nil {
			failures = append(failures, "retained object inventory: "+err.Error())
		} else {
			sample["retainedPinnedObjects"] = retained
		}
		remote, err := nativeRemoteFootprint(observation, store, bucket, prefix)
		cancel()
		if err != nil && ctx.Err() == nil {
			failures = append(failures, "remote inventory: "+err.Error())
		}
		sample["remote"] = remote
		sample["observationMS"] = float64(time.Since(at)) / float64(time.Millisecond)
		sample["errors"] = failures
		sample["complete"] = len(failures) == 0 && ctx.Err() == nil
		if err := encoder.Encode(sample); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Until(at.Add(2 * time.Second))):
		}
	}
}

type nativeLocalSize struct {
	Files, LogicalBytes, AllocatedBytes int64
}

func nativeLocalFootprint(root string) (nativeLocalSize, error) {
	var result nativeLocalSize
	seen := map[[2]uint64]bool{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return nil // Concurrent collection may remove an observed path.
		}
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		stat := info.Sys().(*syscall.Stat_t)
		identity := [2]uint64{uint64(stat.Dev), stat.Ino}
		if seen[identity] {
			return nil
		}
		seen[identity] = true
		result.AllocatedBytes += stat.Blocks * 512
		if !entry.IsDir() {
			result.Files++
			result.LogicalBytes += info.Size()
		}
		return nil
	})
	return result, err
}

type nativeRemoteSize struct {
	Versions, VersionBytes, CurrentVersions, CurrentBytes, DeleteMarkers int64
	MultipartUploads, MultipartParts, MultipartBytes                     int64
	ObserverPageCalls                                                    int64
}

func nativeRemoteFootprint(ctx context.Context, store *awss3.Client, bucket, prefix string) (nativeRemoteSize, error) {
	var result nativeRemoteSize
	versions := awss3.NewListObjectVersionsPaginator(store, &awss3.ListObjectVersionsInput{Bucket: aws.String(bucket), Prefix: aws.String(prefix)})
	for versions.HasMorePages() {
		result.ObserverPageCalls++
		page, err := versions.NextPage(ctx)
		if err != nil {
			return result, err
		}
		for _, version := range page.Versions {
			result.Versions++
			result.VersionBytes += aws.ToInt64(version.Size)
			if aws.ToBool(version.IsLatest) {
				result.CurrentVersions++
				result.CurrentBytes += aws.ToInt64(version.Size)
			}
		}
		result.DeleteMarkers += int64(len(page.DeleteMarkers))
	}
	multipart := awss3.NewListMultipartUploadsPaginator(store, &awss3.ListMultipartUploadsInput{Bucket: aws.String(bucket), Prefix: aws.String(prefix)})
	for multipart.HasMorePages() {
		result.ObserverPageCalls++
		page, err := multipart.NextPage(ctx)
		if err != nil {
			return result, err
		}
		for _, upload := range page.Uploads {
			result.MultipartUploads++
			parts := awss3.NewListPartsPaginator(store, &awss3.ListPartsInput{Bucket: aws.String(bucket), Key: upload.Key, UploadId: upload.UploadId})
			for parts.HasMorePages() {
				result.ObserverPageCalls++
				page, err := parts.NextPage(ctx)
				if err != nil {
					return result, err
				}
				for _, part := range page.Parts {
					result.MultipartParts++
					result.MultipartBytes += aws.ToInt64(part.Size)
				}
			}
		}
	}
	return result, nil
}

// This is a data-availability gate, not a claim of continuous observation or a
// physical peak. Keep all rows and their timestamps so sampling gaps stay visible.
func verifyNativeResourceCoverage(path string, started, finished time.Time) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	complete, vmSamples := 0, 0
	for {
		var sample struct {
			At        time.Time
			Complete  bool
			Errors    []string
			VMCgroups map[string]nativeVMResource
		}
		if err := decoder.Decode(&sample); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return err
		}
		if sample.At.Before(started) || sample.At.After(finished) {
			continue
		}
		if !sample.Complete {
			return fmt.Errorf("incomplete resource sample at %s: %v", sample.At, sample.Errors)
		}
		complete++
		for _, vm := range sample.VMCgroups {
			if len(vm.Processes) > 0 {
				vmSamples++
			}
		}
	}
	if complete == 0 {
		return errors.New("no complete resource sample during native driver execution")
	}
	if vmSamples == 0 {
		return errors.New("no owned VM process resource sample during native driver execution")
	}
	return nil
}

func TestNativeResourceCoverageRejectsMissingAndIncompleteSamples(t *testing.T) {
	start := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name, rows string
		valid      bool
	}{
		{"empty", "", false},
		{"outside", `{"at":"2026-10-09T00:00:00Z","complete":true}`, false},
		{"incomplete", `{"at":"2026-10-10T00:00:01Z","complete":false}`, false},
		{"missing VM", `{"at":"2026-10-10T00:00:01Z","complete":true}`, false},
		{"complete", `{"at":"2026-10-10T00:00:01Z","complete":true,"vmCgroups":{"owner":{"Processes":{"123":{"stat":"observed"}}}}}`, true},
		{"malformed", `{`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "samples.jsonl")
			if err := os.WriteFile(path, []byte(test.rows), 0600); err != nil {
				t.Fatal(err)
			}
			err := verifyNativeResourceCoverage(path, start, start.Add(time.Minute))
			if (err == nil) != test.valid {
				t.Fatalf("coverage error=%v, expected valid=%v", err, test.valid)
			}
		})
	}
}

func TestNativeLocalFootprintCountsHardlinksOnceAndDoesNotFollowSymlinks(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "excluded"), make([]byte, 8192), 0600); err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(root, "first")
	if err := os.WriteFile(first, []byte("abc"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(first, filepath.Join(root, "second")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	got, err := nativeLocalFootprint(root)
	if err != nil {
		t.Fatal(err)
	}
	if got.Files != 2 || got.LogicalBytes != int64(3+len(outside)) {
		t.Fatalf("unexpected footprint: %+v", got)
	}
}

func nativePerformanceStore(ctx context.Context, input nativeExecutionConfig) (*awss3.Client, string, string, error) {
	uri, err := url.Parse(input.CASURI)
	if err != nil {
		return nil, "", "", err
	}
	prefix := strings.TrimSuffix(strings.TrimPrefix(uri.Path, "/"), "/cas") + "/"
	if uri.Scheme != "s3" || !strings.Contains("/"+prefix, "/_verification/native-execution/") || input.PlatformURI != strings.TrimSuffix(input.CASURI, "/cas")+"/platform" {
		return nil, "", "", errors.New("performance inventory requires one exact disposable S3 prefix")
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, "", "", err
	}
	return awss3.NewFromConfig(cfg), uri.Host, prefix, nil
}

func retainNativeRemoteAfterRetirement(input nativeExecutionConfig) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	started := time.Now()
	store, bucket, prefix, err := nativePerformanceStore(ctx, input)
	if err != nil {
		return err
	}
	remote, inventoryErr := nativeRemoteFootprint(ctx, store, bucket, prefix)
	result := map[string]any{"observedAt": started.UTC(), "remote": remote, "observationMS": float64(time.Since(started)) / float64(time.Millisecond), "complete": inventoryErr == nil}
	if inventoryErr != nil {
		result["error"] = inventoryErr.Error()
	}
	raw, err := json.Marshal(result)
	if err == nil {
		err = os.WriteFile(filepath.Join(input.Evidence, "performance-remote-after-retirement.json"), raw, 0600)
	}
	return errors.Join(inventoryErr, err)
}

type nativeVMResource struct {
	Counters    map[string]string
	Processes   map[string]map[string]string
	Unavailable []string
}

// Jailer VMs live outside the Worker cgroup. Discover only owners in this case's
// no-follow custody tree; never attribute unrelated Firecracker groups to it.
// /proc process CPU and resident/high-water memory remain available when the
// shared jailer parent's optional memory/I/O controllers are not enabled.
func nativeVMResources(workRoot, cgroupRoot string) (map[string]nativeVMResource, error) {
	result := map[string]nativeVMResource{}
	owners, err := custody.ReadDirectory(workRoot, "vms", "guest")
	if errors.Is(err, fs.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	state := custody.StateRoot{Base: workRoot, Components: []string{"vms", "guest"}}
	for _, entry := range owners {
		if !entry.IsDir() {
			continue
		}
		owner, err := state.ReadOwner(entry.Name())
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			// Marker creation and writing are separate syscalls. Record the transient
			// boundary instead of treating a launching/reclaimed VM as corrupt data.
			result[entry.Name()] = nativeVMResource{Unavailable: []string{"owner record: " + err.Error()}}
			continue
		}
		group := filepath.Join(cgroupRoot, owner.ID)
		raw, err := os.ReadFile(filepath.Join(group, "cgroup.procs"))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		} // Not launched yet, or already reclaimed.
		if err != nil {
			return result, err
		}
		value := nativeVMResource{Counters: map[string]string{}, Processes: map[string]map[string]string{}}
		for _, name := range []string{"cpu.stat", "memory.current", "memory.peak", "io.stat"} {
			data, err := os.ReadFile(filepath.Join(group, name))
			if errors.Is(err, fs.ErrNotExist) {
				value.Unavailable = append(value.Unavailable, name)
				continue
			}
			if err != nil {
				return result, err
			}
			value.Counters[name] = string(data)
		}
		for _, id := range strings.Fields(string(raw)) {
			pid, err := strconv.Atoi(id)
			if err != nil || pid <= 0 {
				return result, fmt.Errorf("invalid owned VM pid %q", id)
			}
			process := map[string]string{}
			gone := false
			for _, name := range []string{"stat", "status", "io"} {
				data, err := os.ReadFile(fmt.Sprintf("/proc/%d/%s", pid, name))
				if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
					value.Unavailable = append(value.Unavailable, "process "+id+" disappeared during observation")
					gone = true
					break
				}
				if err != nil {
					return result, err
				}
				process[name] = string(data)
			}
			if !gone {
				value.Processes[id] = process
			}
		}
		result[owner.ID] = value
	}
	return result, nil
}

func TestNativeVMResourcesExcludeUnownedGroups(t *testing.T) {
	work, groups := t.TempDir(), t.TempDir()
	id, unrelated := uuid.NewV7().String(), uuid.NewV7().String()
	owner := filepath.Join(work, "vms", "guest", id)
	if err := os.MkdirAll(owner, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(owner, "owner"), []byte("instance\n"+id+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{id, unrelated} {
		path := filepath.Join(groups, name)
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "cgroup.procs"), []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
			t.Fatal(err)
		}
	}
	pendingID := uuid.NewV7().String()
	pending := filepath.Join(work, "vms", "guest", pendingID)
	if err := os.Mkdir(pending, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pending, "owner"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	got, err := nativeVMResources(work, groups)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || len(got[id].Processes) != 1 || len(got[id].Unavailable) != 4 || len(got[pendingID].Unavailable) != 1 {
		t.Fatalf("unexpected VM inventory: %+v", got)
	}
}
