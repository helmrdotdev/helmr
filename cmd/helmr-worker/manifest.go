package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/helmrdotdev/helmr/capacityapi"
)

var sourceCommitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

func runManifest(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("manifest", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var runtimeProfileFile, roles, workerVersion string
	var capacityCPU, capacityMemory, capacityDisk int64
	var perVMCPU, perVMMemory, perVMDisk int64
	var vmSlots, buildExecutors, runtimeStarts int64
	var buildCache, artifactCache int64
	flags.StringVar(&runtimeProfileFile, "runtime-profile-file", "", "attested runtime profile JSON from Worker image production")
	flags.StringVar(&workerVersion, "worker-version", "", "exact source commit injected into the Worker binary")
	flags.StringVar(&roles, "roles", "", "comma-separated run and build roles")
	flags.Int64Var(&capacityCPU, "capacity-cpu-millis", 0, "aggregate allocatable CPU")
	flags.Int64Var(&capacityMemory, "capacity-memory-bytes", 0, "aggregate allocatable memory")
	flags.Int64Var(&capacityDisk, "capacity-guest-disk-bytes", 0, "aggregate allocatable guest disk")
	flags.Int64Var(&perVMCPU, "per-vm-cpu-millis", 0, "per-VM CPU ceiling")
	flags.Int64Var(&perVMMemory, "per-vm-memory-bytes", 0, "per-VM memory ceiling")
	flags.Int64Var(&perVMDisk, "per-vm-guest-disk-bytes", 0, "per-VM guest disk ceiling")
	flags.Int64Var(&vmSlots, "vm-slots", 0, "VM slots")
	flags.Int64Var(&buildExecutors, "build-executors", 0, "build executors")
	flags.Int64Var(&runtimeStarts, "runtime-starts", 0, "concurrent runtime starts")
	flags.Int64Var(&buildCache, "build-cache-bytes", 0, "build/substrate cache")
	flags.Int64Var(&artifactCache, "artifact-cache-bytes", 0, "artifact cache")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 || strings.TrimSpace(runtimeProfileFile) == "" {
		return errors.New("runtime-profile-file and only supported flags are required")
	}
	if !sourceCommitPattern.MatchString(workerVersion) {
		return errors.New("worker-version must be an exact lowercase 40-character Git commit")
	}
	roleSet := map[string]bool{}
	for _, role := range strings.Split(roles, ",") {
		role = strings.TrimSpace(role)
		if role != "run" && role != "build" {
			return fmt.Errorf("unsupported Worker role %q", role)
		}
		roleSet[role] = true
	}
	runtimeProfile, err := readRuntimeProfile(runtimeProfileFile)
	if err != nil {
		return fmt.Errorf("read runtime profile: %w", err)
	}
	manifest := capacityapi.WorkerReleaseManifest{
		Schema: capacityapi.WorkerReleaseManifestSchema, WorkerVersion: workerVersion,
		SupportsRun: roleSet["run"], SupportsBuild: roleSet["build"], Runtime: runtimeProfile,
		Capacity: capacityapi.ResourceVector{
			CPUMillis: capacityCPU, MemoryBytes: capacityMemory, GuestEphemeralDiskBytes: capacityDisk,
			VMSlots: vmSlots, BuildExecutors: buildExecutors,
		},
		PerVM:            capacityapi.ResourceVector{CPUMillis: perVMCPU, MemoryBytes: perVMMemory, GuestEphemeralDiskBytes: perVMDisk},
		MaxRuntimeStarts: runtimeStarts, BuildCacheBytes: buildCache, ArtifactCacheBytes: artifactCache,
	}
	if manifest.SupportsRun {
		manifest.Substrate = capacityapi.SubstrateProfile{Format: capacityapi.SubstrateFormatExt4, Contract: capacityapi.SubstrateContractExt4}
	}
	manifest.ReleaseFingerprint, err = manifest.ExpectedFingerprint()
	if err != nil {
		return err
	}
	if err := manifest.Validate(); err != nil {
		return err
	}
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	return encoder.Encode(manifest)
}

func readRuntimeProfile(path string) (capacityapi.RuntimeProfile, error) {
	file, err := os.Open(path)
	if err != nil {
		return capacityapi.RuntimeProfile{}, err
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var profile capacityapi.RuntimeProfile
	if err := decoder.Decode(&profile); err != nil {
		return capacityapi.RuntimeProfile{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return capacityapi.RuntimeProfile{}, errors.New("runtime profile must contain one JSON value")
	}
	expectedID, err := profile.ExpectedID()
	if err != nil {
		return capacityapi.RuntimeProfile{}, err
	}
	if profile.ID != expectedID {
		return capacityapi.RuntimeProfile{}, errors.New("runtime profile ID does not match its artifact identity")
	}
	return profile, nil
}
