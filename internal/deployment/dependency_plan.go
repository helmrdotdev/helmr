package deployment

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

const (
	DependencyPlanFormatVersion = 0
	maxDependencyPlanBytes      = 64 << 10
	dependencyPlanDigestDomain  = "helmr.dependency-execution-plan.v0\x00"
)

type PlanAlias struct {
	Path   string `json:"path"`
	Target string `json:"target"`
}

type PlanEnvironment struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type PlanCommand struct {
	Argv []string `json:"argv"`
	CWD  string   `json:"cwd"`
}

type PlanIdentity struct {
	GID int64 `json:"gid"`
	UID int64 `json:"uid"`
}

type PlanLimits struct {
	CPUPeriodMicros int64 `json:"cpuPeriodMicros"`
	CPUQuotaMicros  int64 `json:"cpuQuotaMicros"`
	MaxOutputBytes  int64 `json:"maxOutputBytes"`
	MemoryBytes     int64 `json:"memoryBytes"`
	PIDs            int64 `json:"pids"`
	ScratchBytes    int64 `json:"scratchBytes"`
	WallTimeSeconds int64 `json:"wallTimeSeconds"`
}

type PlanMounts struct {
	Manager           string `json:"manager"`
	Project           string `json:"project"`
	Runtime           string `json:"runtime"`
	StandardToolchain string `json:"standardToolchain"`
}

type PlanOfflineStore struct {
	ReadOnlyMountPath string `json:"readOnlyMountPath"`
	WorkPath          string `json:"workPath"`
}

type PlanOutput struct {
	FormatVersion    int    `json:"formatVersion"`
	MaxMetadataBytes int64  `json:"maxMetadataBytes"`
	TreeFormat       string `json:"treeFormat"`
}

type PlanProxy struct {
	RegistryOrigin string `json:"registryOrigin"`
}

type DependencyPlan struct {
	Aliases                 []PlanAlias         `json:"aliases"`
	Architecture            RuntimeArchitecture `json:"architecture"`
	Environment             []PlanEnvironment   `json:"environment"`
	FormatVersion           int                 `json:"formatVersion"`
	Handshake               PlanCommand         `json:"handshake"`
	Identity                PlanIdentity        `json:"identity"`
	Lifecycle               PlanCommand         `json:"lifecycle"`
	Limits                  PlanLimits          `json:"limits"`
	ManagedRuntimeDigest    string              `json:"managedRuntimeDigest"`
	ManagerCapsuleDigest    string              `json:"managerCapsuleDigest"`
	MaterializerVersion     string              `json:"materializerVersion"`
	Mounts                  PlanMounts          `json:"mounts"`
	OfflineStore            PlanOfflineStore    `json:"offlineStore"`
	Output                  PlanOutput          `json:"output"`
	PackageManager          PackageManager      `json:"packageManager"`
	Probe                   PlanCommand         `json:"probe"`
	Proxy                   PlanProxy           `json:"proxy"`
	Resolution              PlanCommand         `json:"resolution"`
	StandardToolchainDigest string              `json:"standardToolchainDigest"`
}

func NewDependencyPlan(
	capsule ManagerCapsule,
	toolchain Toolchain,
	materializerVersion string,
) (DependencyPlan, error) {
	if err := validateManagerCapsule(capsule); err != nil {
		return DependencyPlan{}, err
	}
	if err := validateToolchain(toolchain); err != nil {
		return DependencyPlan{}, err
	}
	if capsule.Architecture != toolchain.Architecture {
		return DependencyPlan{}, errors.New(
			"manager capsule and standard toolchain architectures do not match",
		)
	}
	if materializerVersion != DependencyMaterializerVersion {
		return DependencyPlan{}, fmt.Errorf(
			"materializerVersion = %q, want %q",
			materializerVersion,
			DependencyMaterializerVersion,
		)
	}
	capsuleDigest, err := ManagerCapsuleDigest(capsule)
	if err != nil {
		return DependencyPlan{}, err
	}
	toolchainDigest, err := StandardToolchainDigest(toolchain)
	if err != nil {
		return DependencyPlan{}, err
	}
	return dependencyPlanTemplate(
		capsule.PackageManager,
		capsule.Architecture,
		capsuleDigest,
		toolchain.ManagedRuntimeDigest,
		toolchainDigest,
		materializerVersion,
	)
}

func ParseDependencyPlan(raw []byte) (DependencyPlan, error) {
	if len(raw) == 0 || len(raw) > maxDependencyPlanBytes {
		return DependencyPlan{}, fmt.Errorf(
			"dependency plan size is outside [1,%d]",
			maxDependencyPlanBytes,
		)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return DependencyPlan{}, fmt.Errorf("canonicalize dependency plan: %w", err)
	}
	if !bytes.Equal(raw, canonical) {
		return DependencyPlan{}, errors.New(
			"dependency plan is not RFC 8785 canonical JSON",
		)
	}

	var plan DependencyPlan
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&plan); err != nil {
		return DependencyPlan{}, fmt.Errorf("decode dependency plan: %w", err)
	}
	if err := ensureEOF(decoder, "dependency plan"); err != nil {
		return DependencyPlan{}, err
	}
	if err := ValidateDependencyPlan(plan); err != nil {
		return DependencyPlan{}, err
	}
	complete, err := CanonicalDependencyPlan(plan)
	if err != nil {
		return DependencyPlan{}, err
	}
	if !bytes.Equal(raw, complete) {
		return DependencyPlan{}, errors.New(
			"dependency plan does not match the complete canonical v0 shape",
		)
	}
	return plan, nil
}

func CanonicalDependencyPlan(plan DependencyPlan) ([]byte, error) {
	if err := ValidateDependencyPlan(plan); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		return nil, fmt.Errorf("encode dependency plan: %w", err)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return nil, fmt.Errorf("canonicalize dependency plan: %w", err)
	}
	if len(canonical) == 0 || len(canonical) > maxDependencyPlanBytes {
		return nil, fmt.Errorf(
			"dependency plan size is outside [1,%d]",
			maxDependencyPlanBytes,
		)
	}
	return canonical, nil
}

func DependencyPlanDigest(plan DependencyPlan) (string, error) {
	canonical, err := CanonicalDependencyPlan(plan)
	if err != nil {
		return "", err
	}
	digest := domainDigest(dependencyPlanDigestDomain, canonical)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func ValidateDependencyPlan(plan DependencyPlan) error {
	if plan.FormatVersion != DependencyPlanFormatVersion {
		return fmt.Errorf(
			"dependency plan formatVersion = %d, want %d",
			plan.FormatVersion,
			DependencyPlanFormatVersion,
		)
	}
	if err := validateManagerPackage(plan.PackageManager); err != nil {
		return err
	}
	if !validArchitecture(plan.Architecture) {
		return fmt.Errorf(
			"dependency plan architecture %q is unsupported",
			plan.Architecture,
		)
	}
	if !sha256DigestPattern.MatchString(plan.ManagerCapsuleDigest) {
		return errors.New(
			"dependency plan managerCapsuleDigest is not a lowercase SHA-256 digest",
		)
	}
	if !sha256DigestPattern.MatchString(plan.ManagedRuntimeDigest) {
		return errors.New(
			"dependency plan managedRuntimeDigest is not a lowercase SHA-256 digest",
		)
	}
	if !sha256DigestPattern.MatchString(plan.StandardToolchainDigest) {
		return errors.New(
			"dependency plan standardToolchainDigest is not a lowercase SHA-256 digest",
		)
	}
	if plan.MaterializerVersion != DependencyMaterializerVersion {
		return fmt.Errorf(
			"dependency plan materializerVersion = %q, want %q",
			plan.MaterializerVersion,
			DependencyMaterializerVersion,
		)
	}
	expected, err := dependencyPlanTemplate(
		plan.PackageManager,
		plan.Architecture,
		plan.ManagerCapsuleDigest,
		plan.ManagedRuntimeDigest,
		plan.StandardToolchainDigest,
		plan.MaterializerVersion,
	)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(plan, expected) {
		return errors.New(
			"dependency plan does not match the closed package-manager template",
		)
	}
	return nil
}

func dependencyPlanTemplate(
	manager PackageManager,
	architecture RuntimeArchitecture,
	managerCapsuleDigest,
	managedRuntimeDigest,
	standardToolchainDigest,
	materializerVersion string,
) (DependencyPlan, error) {
	var probe, handshake, resolution, lifecycle PlanCommand
	aliases := []PlanAlias{
		{Path: "/bin/sh", Target: "/nix/bin/sh"},
		{Path: "/usr/bin/env", Target: "/nix/bin/env"},
	}
	environment := []PlanEnvironment{
		{Name: "HOME", Value: "/work/home"},
		{
			Name:  "PATH",
			Value: "/opt/helmr/manager/bin:/opt/helmr/runtime/bin:/nix/bin",
		},
	}
	switch manager.Name {
	case PackageManagerBun:
		cpu := "arm64"
		interpreter := PlanAlias{
			Path:   "/lib/ld-linux-aarch64.so.1",
			Target: "/nix/helmr/manager/lib/ld-linux-aarch64.so.1",
		}
		switch architecture {
		case ArchitectureAArch64:
		case ArchitectureX8664:
			cpu = "x64"
			interpreter = PlanAlias{
				Path:   "/lib64/ld-linux-x86-64.so.2",
				Target: "/nix/helmr/manager/lib/ld-linux-x86-64.so.2",
			}
		default:
			return DependencyPlan{}, fmt.Errorf(
				"dependency plan architecture %q is unsupported",
				architecture,
			)
		}
		aliases = []PlanAlias{aliases[0], interpreter, aliases[1]}
		environment = []PlanEnvironment{
			environment[0],
			{
				Name:  "LD_LIBRARY_PATH",
				Value: "/nix/helmr/manager/lib",
			},
			environment[1],
		}
		probe = PlanCommand{
			Argv: []string{managerBunEntrypoint, "--version"},
			CWD:  "/work",
		}
		handshake = PlanCommand{
			Argv: []string{managerBunEntrypoint, "install", "--help"},
			CWD:  "/work",
		}
		common := []string{
			managerBunEntrypoint,
			"install",
			"--frozen-lockfile",
			"--omit=dev",
		}
		resolution = PlanCommand{
			Argv: append(append([]string(nil), common...),
				"--ignore-scripts",
				"--no-save",
				"--no-progress",
				"--no-summary",
				"--cache-dir=/work/offline-store",
				"--registry=http://127.0.0.1:4873",
				"--linker=hoisted",
				"--backend=copyfile",
				"--cpu="+cpu,
				"--os=linux",
			),
			CWD: "/work/project",
		}
		lifecycle = PlanCommand{
			Argv: append(append([]string(nil), common...),
				"--no-save",
				"--no-progress",
				"--no-summary",
				"--cache-dir=/work/offline-store",
				"--registry=http://127.0.0.1:4873",
				"--linker=hoisted",
				"--backend=copyfile",
				"--cpu="+cpu,
				"--os=linux",
				"--concurrent-scripts=1",
			),
			CWD: "/work/project",
		}
	case PackageManagerNPM:
		prefix := []string{
			"/opt/helmr/runtime/bin/node",
			managerNPMEntrypoint,
		}
		probe = PlanCommand{
			Argv: append(append([]string(nil), prefix...), "--version"),
			CWD:  "/work",
		}
		handshake = PlanCommand{
			Argv: append(append([]string(nil), prefix...), "ci", "--help"),
			CWD:  "/work",
		}
		common := []string{
			"ci",
			"--omit=dev",
			"--audit=false",
			"--fund=false",
			"--update-notifier=false",
			"--progress=false",
			"--install-strategy=hoisted",
			"--legacy-peer-deps=false",
			"--strict-peer-deps=false",
			"--userconfig=/work/config/npmrc",
			"--globalconfig=/work/config/global-npmrc",
			"--cache=/work/offline-store",
			"--registry=http://127.0.0.1:4873",
			"--workspaces=true",
			"--include-workspace-root=true",
		}
		resolution = PlanCommand{
			Argv: append(
				append(append([]string(nil), prefix...), "ci", "--ignore-scripts=true"),
				common[1:]...,
			),
			CWD: "/work/project",
		}
		lifecycle = PlanCommand{
			Argv: append(
				append(
					append([]string(nil), prefix...),
					"ci",
					"--ignore-scripts=false",
					"--offline=true",
					"--foreground-scripts=true",
				),
				common[1:]...,
			),
			CWD: "/work/project",
		}
	default:
		return DependencyPlan{}, fmt.Errorf(
			"package manager %q is unsupported",
			manager.Name,
		)
	}

	return DependencyPlan{
		Aliases:       aliases,
		Architecture:  architecture,
		Environment:   environment,
		FormatVersion: DependencyPlanFormatVersion,
		Handshake:     handshake,
		Identity: PlanIdentity{
			GID: 65532,
			UID: 65532,
		},
		Lifecycle: lifecycle,
		Limits: PlanLimits{
			CPUPeriodMicros: 100000,
			CPUQuotaMicros:  200000,
			MaxOutputBytes:  9 << 30,
			MemoryBytes:     2 << 30,
			PIDs:            512,
			ScratchBytes:    20 << 30,
			WallTimeSeconds: 1800,
		},
		ManagedRuntimeDigest: managedRuntimeDigest,
		ManagerCapsuleDigest: managerCapsuleDigest,
		MaterializerVersion:  materializerVersion,
		Mounts: PlanMounts{
			Manager:           "/opt/helmr/manager",
			Project:           "/opt/helmr/project",
			Runtime:           "/opt/helmr/runtime",
			StandardToolchain: "/nix",
		},
		OfflineStore: PlanOfflineStore{
			ReadOnlyMountPath: "/opt/helmr/offline-store",
			WorkPath:          "/work/offline-store",
		},
		Output: PlanOutput{
			FormatVersion:    0,
			MaxMetadataBytes: (16 << 20) + (64 << 10),
			TreeFormat:       "helmr.tar.v0",
		},
		PackageManager: manager,
		Probe:          probe,
		Proxy: PlanProxy{
			RegistryOrigin: "http://127.0.0.1:4873",
		},
		Resolution:              resolution,
		StandardToolchainDigest: standardToolchainDigest,
	}, nil
}
