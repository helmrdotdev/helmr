package worker

import (
	"errors"
	"io/fs"
	"strings"
	"testing"
)

type fakeStorageProbe struct {
	files       map[string]storageFile
	fileData    map[string][]byte
	fileError   map[string]error
	filesystems map[string]storageFS
}

func (p *fakeStorageProbe) Lstat(path string) (storageFile, error) {
	if file, ok := p.files[path]; ok {
		return file, nil
	}
	return storageFile{Kind: storageFileDirectory}, nil
}

func (p *fakeStorageProbe) StatFS(path string) (storageFS, error) {
	stat, ok := p.filesystems[path]
	if !ok {
		return storageFS{}, fs.ErrNotExist
	}
	return stat, nil
}

func (p *fakeStorageProbe) ReadFile(path string) ([]byte, error) {
	if err, ok := p.fileError[path]; ok {
		return nil, err
	}
	if data, ok := p.fileData[path]; ok {
		return data, nil
	}
	return nil, fs.ErrNotExist
}

func validStorageFixture() (BuildStorageConfig, *fakeStorageProbe) {
	cacheDevice := "8:1"
	scratchDevice := "8:2"
	config := BuildStorageConfig{
		CacheRoot:            "/cache",
		ScratchRoot:          "/scratch",
		WorkDir:              "/scratch/worker",
		JailerRoot:           "/scratch/jailer",
		RequiredCacheBytes:   8 << 30,
		RequiredScratchBytes: 32 << 30,
	}
	probe := &fakeStorageProbe{
		files: map[string]storageFile{
			"/cache":       {Kind: storageFileDirectory, Device: cacheDevice},
			"/scratch":     {Kind: storageFileDirectory, Device: scratchDevice},
			"/dev/cache":   {Kind: storageFileBlock, RDev: cacheDevice},
			"/dev/scratch": {Kind: storageFileBlock, RDev: scratchDevice},
		},
		fileData: map[string][]byte{
			"/proc/self/mountinfo": []byte(strings.Join([]string{
				"31 1 8:1 / /cache rw,relatime - ext4 /dev/cache rw,nodiscard",
				"32 1 8:2 / /scratch rw,relatime - ext4 /dev/scratch rw,nodiscard",
			}, "\n")),
		},
		fileError: map[string]error{
			"/sys/dev/block/8:1/loop/backing_file": fs.ErrNotExist,
			"/sys/dev/block/8:2/loop/backing_file": fs.ErrNotExist,
		},
		filesystems: map[string]storageFS{
			"/cache":   {Blocks: 16 << 20, Free: 10 << 20, Available: 10 << 20, BlockSize: 4096},
			"/scratch": {Blocks: 10 << 20, Free: 9 << 20, Available: 9 << 20, BlockSize: 4096},
		},
	}
	return config, probe
}

func TestProveBuildStorage(t *testing.T) {
	config, probe := validStorageFixture()
	proof, err := proveBuildStorage(config, probe)
	if err != nil {
		t.Fatal(err)
	}
	if proof.Cache.Root != "/cache" || proof.Cache.MountID != 31 || proof.Cache.Source != "/dev/cache" {
		t.Fatalf("unexpected cache proof: %+v", proof.Cache)
	}
	if proof.Scratch.Root != "/scratch" || proof.Scratch.MountID != 32 || proof.Scratch.Source != "/dev/scratch" {
		t.Fatalf("unexpected scratch proof: %+v", proof.Scratch)
	}
	if proof.BuildKitRoot != "/cache/buildkit" ||
		proof.SubstrateCacheDir != "/cache/substrate-cache" ||
		proof.ArtifactCacheDir != "/cache/artifact-cache" {
		t.Fatalf("unexpected cache layout: %+v", proof)
	}
}

func TestProveBuildStorageDoesNotInspectCallerOwnedCacheDirectories(t *testing.T) {
	config, probe := validStorageFixture()
	probe.files["/cache/substrate-cache"] = storageFile{Kind: storageFileSymlink}
	probe.files["/cache/artifact-cache"] = storageFile{Kind: storageFileSymlink}
	if _, err := proveBuildStorage(config, probe); err != nil {
		t.Fatal(err)
	}
}

func TestProveBuildStorageAllowsOccupiedPersistentCache(t *testing.T) {
	config, probe := validStorageFixture()
	probe.filesystems["/cache"] = storageFS{
		Blocks: 16 << 20, Free: 1, Available: 1, BlockSize: 4096,
	}
	if _, err := proveBuildStorage(config, probe); err != nil {
		t.Fatal(err)
	}
}

func TestProveBuildStorageRejectsInvalidBoundary(t *testing.T) {
	tests := []struct {
		name   string
		change func(*BuildStorageConfig, *fakeStorageProbe)
		want   string
	}{
		{
			name: "missing root",
			change: func(config *BuildStorageConfig, _ *fakeStorageProbe) {
				config.CacheRoot = ""
			},
			want: "build cache root is required",
		},
		{
			name: "relative root",
			change: func(config *BuildStorageConfig, _ *fakeStorageProbe) {
				config.CacheRoot = "cache"
			},
			want: "build cache root must be absolute",
		},
		{
			name: "symlink root",
			change: func(_ *BuildStorageConfig, probe *fakeStorageProbe) {
				probe.files["/cache"] = storageFile{Kind: storageFileSymlink}
			},
			want: `build cache root component "/cache" is a symlink`,
		},
		{
			name: "symlink layout",
			change: func(_ *BuildStorageConfig, probe *fakeStorageProbe) {
				probe.files["/cache/buildkit"] = storageFile{Kind: storageFileSymlink}
			},
			want: `BuildKit root component "/cache/buildkit" is a symlink`,
		},
		{
			name: "work outside scratch",
			change: func(config *BuildStorageConfig, _ *fakeStorageProbe) {
				config.WorkDir = "/worker"
			},
			want: "worker work directory must be a strict descendant of build scratch",
		},
		{
			name: "jailer equals scratch",
			change: func(config *BuildStorageConfig, _ *fakeStorageProbe) {
				config.JailerRoot = "/scratch"
			},
			want: "firecracker jailer root must be a strict descendant of build scratch",
		},
		{
			name: "root is not mountpoint",
			change: func(_ *BuildStorageConfig, probe *fakeStorageProbe) {
				probe.fileData["/proc/self/mountinfo"] = []byte(
					"32 1 8:2 / /scratch rw - ext4 /dev/scratch rw,nodiscard",
				)
			},
			want: `"/cache" must identify exactly one mountpoint`,
		},
		{
			name: "root is bind mount",
			change: func(_ *BuildStorageConfig, probe *fakeStorageProbe) {
				probe.fileData["/proc/self/mountinfo"] = []byte(strings.Join([]string{
					"31 1 8:1 /nested /cache rw - ext4 /dev/cache rw,nodiscard",
					"32 1 8:2 / /scratch rw - ext4 /dev/scratch rw,nodiscard",
				}, "\n"))
			},
			want: `"/cache" is a bind or subdirectory mount`,
		},
		{
			name: "shared mount ID",
			change: func(_ *BuildStorageConfig, probe *fakeStorageProbe) {
				probe.fileData["/proc/self/mountinfo"] = []byte(strings.Join([]string{
					"31 1 8:1 / /cache rw - ext4 /dev/cache rw,nodiscard",
					"31 1 8:2 / /scratch rw - ext4 /dev/scratch rw,nodiscard",
				}, "\n"))
			},
			want: "build cache and scratch share a mount ID",
		},
		{
			name: "shared device",
			change: func(_ *BuildStorageConfig, probe *fakeStorageProbe) {
				probe.files["/scratch"] = storageFile{Kind: storageFileDirectory, Device: "8:1"}
				probe.fileData["/proc/self/mountinfo"] = []byte(strings.Join([]string{
					"31 1 8:1 / /cache rw - ext4 /dev/cache rw,nodiscard",
					"32 1 8:1 / /scratch rw - ext4 /dev/cache rw,nodiscard",
				}, "\n"))
			},
			want: "build cache and scratch share a device",
		},
		{
			name: "discard enabled",
			change: func(_ *BuildStorageConfig, probe *fakeStorageProbe) {
				probe.fileData["/proc/self/mountinfo"] = []byte(strings.Join([]string{
					"31 1 8:1 / /cache rw - ext4 /dev/cache rw,discard",
					"32 1 8:2 / /scratch rw - ext4 /dev/scratch rw,nodiscard",
				}, "\n"))
			},
			want: `"/cache" enables discard`,
		},
		{
			name: "insufficient cache",
			change: func(_ *BuildStorageConfig, probe *fakeStorageProbe) {
				probe.filesystems["/cache"] = storageFS{Blocks: 1, Free: 1, Available: 1, BlockSize: 4096}
			},
			want: "bytes of usable capacity; need",
		},
		{
			name: "insufficient available scratch",
			change: func(_ *BuildStorageConfig, probe *fakeStorageProbe) {
				probe.filesystems["/scratch"] = storageFS{
					Blocks: 10 << 20, Free: 1, Available: 1, BlockSize: 4096,
				}
			},
			want: "available bytes; need",
		},
		{
			name: "reserved filesystem blocks",
			change: func(_ *BuildStorageConfig, probe *fakeStorageProbe) {
				probe.filesystems["/cache"] = storageFS{
					Blocks: 16 << 20, Free: 10 << 20, Available: 9 << 20, BlockSize: 4096,
				}
			},
			want: "reserves filesystem blocks",
		},
		{
			name: "source symlink",
			change: func(_ *BuildStorageConfig, probe *fakeStorageProbe) {
				probe.files["/dev/cache"] = storageFile{Kind: storageFileSymlink}
			},
			want: `source component "/dev/cache" is a symlink`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config, probe := validStorageFixture()
			test.change(&config, probe)
			_, err := proveBuildStorage(config, probe)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("got %v, want error containing %q", err, test.want)
			}
		})
	}
}

func TestProveBuildStorageValidatesLoopBackingAllocation(t *testing.T) {
	config, probe := validStorageFixture()
	cacheDevice := "7:1"
	probe.files["/cache"] = storageFile{Kind: storageFileDirectory, Device: cacheDevice}
	probe.files["/dev/loop1"] = storageFile{Kind: storageFileBlock, RDev: cacheDevice}
	probe.files["/var/lib/helmr/storage/build-cache.ext4"] = storageFile{
		Kind: storageFileRegular, Size: 8192, Blocks: 16,
	}
	probe.fileData["/proc/self/mountinfo"] = []byte(strings.Join([]string{
		"31 1 7:1 / /cache rw - ext4 /dev/loop1 rw,nodiscard",
		"32 1 8:2 / /scratch rw - ext4 /dev/scratch rw,nodiscard",
	}, "\n"))
	delete(probe.fileError, "/sys/dev/block/8:1/loop/backing_file")
	probe.fileData["/sys/dev/block/7:1/loop/backing_file"] = []byte("/var/lib/helmr/storage/build-cache.ext4\n")

	proof, err := proveBuildStorage(config, probe)
	if err != nil {
		t.Fatal(err)
	}
	if proof.Cache.Source != "/var/lib/helmr/storage/build-cache.ext4" {
		t.Fatalf("unexpected cache source %q", proof.Cache.Source)
	}

	probe.files["/var/lib/helmr/storage/build-cache.ext4"] = storageFile{
		Kind: storageFileRegular, Size: 8192, Blocks: 1,
	}
	_, err = proveBuildStorage(config, probe)
	if err == nil || !strings.Contains(err.Error(), "allocation does not equal its size") {
		t.Fatalf("got %v, want backing allocation error", err)
	}
}

func TestParseMountInfoRejectsUnknownEscapes(t *testing.T) {
	_, err := parseMountInfo([]byte(`31 1 8:1 / /cache\777 rw - ext4 /dev/cache rw`))
	if err == nil || !strings.Contains(err.Error(), "unsupported escape") {
		t.Fatalf("got %v, want unsupported escape error", err)
	}
}

func TestProveBuildStorageRejectsProbeFailure(t *testing.T) {
	config, probe := validStorageFixture()
	probe.fileError["/proc/self/mountinfo"] = errors.New("unavailable")
	_, err := proveBuildStorage(config, probe)
	if err == nil || !strings.Contains(err.Error(), "read mountinfo") {
		t.Fatalf("got %v, want mountinfo error", err)
	}
}
