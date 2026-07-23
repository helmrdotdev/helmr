package imagebuild

import (
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	build := validBuild()
	if err := Validate(build, "x86_64"); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got := StepCount(build); got != 10 {
		t.Fatalf("StepCount = %d, want 10", got)
	}
}

func TestValidateRejectsInvalidShape(t *testing.T) {
	tests := []struct {
		name   string
		change func(*Build)
		errMsg string
	}{
		{
			name: "format version",
			change: func(build *Build) {
				build.FormatVersion = 1
			},
			errMsg: "formatVersion",
		},
		{
			name: "root",
			change: func(build *Build) {
				build.Root = "missing"
			},
			errMsg: "does not name an image",
		},
		{
			name: "image order",
			change: func(build *Build) {
				build.Images[0], build.Images[1] = build.Images[1], build.Images[0]
			},
			errMsg: "sorted by key",
		},
		{
			name: "platform",
			change: func(build *Build) {
				build.Images[0].Platform.OS = "darwin"
			},
			errMsg: "platform.os",
		},
		{
			name: "architecture",
			change: func(build *Build) {
				build.Images[0].Platform.Architecture = "aarch64"
			},
			errMsg: "architecture",
		},
		{
			name: "first step",
			change: func(build *Build) {
				build.Images[0].Steps = build.Images[0].Steps[1:]
			},
			errMsg: "first step must be from",
		},
		{
			name: "second from",
			change: func(build *Build) {
				build.Images[0].Steps[1] = Step{
					From: &From{Ref: "alpine:3.23"},
				}
			},
			errMsg: "more than one from",
		},
		{
			name: "empty operation",
			change: func(build *Build) {
				build.Images[0].Steps[1] = Step{}
			},
			errMsg: "exactly one operation",
		},
		{
			name: "multiple operations",
			change: func(build *Build) {
				build.Images[0].Steps[1].Env = &Env{Key: "A", Value: "b"}
			},
			errMsg: "exactly one operation",
		},
		{
			name: "unknown image",
			change: func(build *Build) {
				build.Images[0].Steps[8].CopyFromImage.ImageKey = "missing"
			},
			errMsg: "unknown image",
		},
		{
			name: "cycle",
			change: func(build *Build) {
				build.Images[1].Steps = append(build.Images[1].Steps, Step{
					CopyFromImage: &CopyFromImage{
						Dst:      "/cycle",
						ImageKey: "app",
						SrcPath:  "/cycle",
					},
				})
			},
			errMsg: "cycle",
		},
		{
			name: "unreachable image",
			change: func(build *Build) {
				build.Images = append(build.Images, Spec{
					Key:      "unused",
					Platform: Platform{OS: "linux", Architecture: "x86_64"},
					Steps: []Step{{
						From: &From{Ref: "alpine:3.23"},
					}},
				})
			},
			errMsg: "unreachable",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			build := validBuild()
			test.change(&build)
			assertBuildError(t, build, "x86_64", test.errMsg)
		})
	}
}

func TestValidateRejectsInvalidOperations(t *testing.T) {
	tests := []struct {
		name   string
		change func(*Build)
		errMsg string
	}{
		{
			name: "reference",
			change: func(build *Build) {
				build.Images[0].Steps[0].From.Ref = " Invalid "
			},
			errMsg: "from.ref",
		},
		{
			name: "empty argv",
			change: func(build *Build) {
				build.Images[0].Steps[1].Run.Argv = []string{}
			},
			errMsg: "argv count",
		},
		{
			name: "empty executable",
			change: func(build *Build) {
				build.Images[0].Steps[1].Run.Argv[0] = ""
			},
			errMsg: "argv[0]",
		},
		{
			name: "nil cache mounts",
			change: func(build *Build) {
				build.Images[0].Steps[1].Run.CacheMounts = nil
			},
			errMsg: "cacheMounts must be an array",
		},
		{
			name: "duplicate mount destination",
			change: func(build *Build) {
				build.Images[0].Steps[1].Run.SecretMounts[0].Dst = "/cache"
			},
			errMsg: "duplicate mount destination",
		},
		{
			name: "cache sharing",
			change: func(build *Build) {
				build.Images[0].Steps[1].Run.CacheMounts[0].Sharing = "exclusive"
			},
			errMsg: "sharing is unsupported",
		},
		{
			name: "secret name",
			change: func(build *Build) {
				build.Images[0].Steps[1].Run.SecretMounts[0].Name = "../TOKEN"
			},
			errMsg: "name is invalid",
		},
		{
			name: "destination path",
			change: func(build *Build) {
				build.Images[0].Steps[2].CopySourceFile.Dst = "app"
			},
			errMsg: "absolute POSIX path",
		},
		{
			name: "source path",
			change: func(build *Build) {
				build.Images[0].Steps[2].CopySourceFile.Path = "../package.json"
			},
			errMsg: "Deployment-relative",
		},
		{
			name: "directory dot path",
			change: func(build *Build) {
				build.Images[0].Steps[3].CopySourceDir.Path = "."
			},
		},
		{
			name: "file dot path",
			change: func(build *Build) {
				build.Images[0].Steps[2].CopySourceFile.Path = "."
			},
			errMsg: "Deployment-relative",
		},
		{
			name: "workdir parent",
			change: func(build *Build) {
				build.Images[0].Steps[4].Workdir.Path = "../app"
			},
			errMsg: "clean POSIX path",
		},
		{
			name: "user",
			change: func(build *Build) {
				build.Images[0].Steps[5].User.Name = "root:root:root"
			},
			errMsg: "OCI user",
		},
		{
			name: "environment key",
			change: func(build *Build) {
				build.Images[0].Steps[6].Env.Key = "1INVALID"
			},
			errMsg: "env.key",
		},
		{
			name: "environment aggregate",
			change: func(build *Build) {
				build.Images[0].Steps[6].Env.Value = strings.Repeat("x", maxEnvValueBytes)
			},
			errMsg: "environment exceeds",
		},
		{
			name: "source image path",
			change: func(build *Build) {
				build.Images[0].Steps[8].CopyFromImage.SrcPath = "/out/../secret"
			},
			errMsg: "absolute POSIX path",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			build := validBuild()
			test.change(&build)
			err := Validate(build, "x86_64")
			if test.errMsg == "" {
				if err != nil {
					t.Fatalf("Validate: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.errMsg) {
				t.Fatalf("error = %v, want containing %q", err, test.errMsg)
			}
		})
	}
}

func TestValidateEnforcesIndependentBounds(t *testing.T) {
	t.Run("image key", func(t *testing.T) {
		build := validBuild()
		build.Root = strings.Repeat("a", maxImageIdentifierBytes+1)
		assertBuildError(t, build, "x86_64", "root")
	})
	t.Run("argument", func(t *testing.T) {
		build := validBuild()
		build.Images[0].Steps[1].Run.Argv = []string{strings.Repeat("a", maxImageArgumentBytes+1)}
		assertBuildError(t, build, "x86_64", "argv[0]")
	})
	t.Run("mount count", func(t *testing.T) {
		build := validBuild()
		build.Images[0].Steps[1].Run.CacheMounts = make([]CacheMount, maxImageMounts+1)
		assertBuildError(t, build, "x86_64", "cache mounts")
	})
	t.Run("step count", func(t *testing.T) {
		build := validBuild()
		steps := make([]Step, maxBuildSteps+1)
		steps[0] = Step{From: &From{Ref: "alpine:3.23"}}
		for index := 1; index < len(steps); index++ {
			steps[index] = Step{Workdir: &Workdir{Path: "."}}
		}
		build.Images = []Spec{{
			Key:      "app",
			Platform: Platform{OS: "linux", Architecture: "x86_64"},
			Steps:    steps,
		}}
		assertBuildError(t, build, "x86_64", "more than 10000 steps")
	})
}

func validBuild() Build {
	return Build{
		FormatVersion: FormatVersion,
		Root:          "app",
		Images: []Spec{
			{
				Key:      "app",
				Platform: Platform{OS: "linux", Architecture: "x86_64"},
				Steps: []Step{
					{From: &From{Ref: "debian:bookworm-slim"}},
					{Run: &Run{
						Argv: []string{"sh", "-c", "mkdir -p /app /out"},
						CacheMounts: []CacheMount{{
							Dst:     "/cache",
							CacheID: "pnpm",
							Sharing: "locked",
						}},
						SecretMounts: []SecretMount{{
							Dst:  "/run/secrets/TOKEN",
							Name: "TOKEN",
						}},
					}},
					{CopySourceFile: &CopySourceFile{
						Dst:  "/app/package.json",
						Path: "package.json",
					}},
					{CopySourceDir: &CopySourceDir{
						Dst:  "/app/src",
						Path: "src",
					}},
					{Workdir: &Workdir{Path: "/app"}},
					{User: &User{Name: "1000:1000"}},
					{Env: &Env{Key: "NODE_ENV", Value: "production"}},
					{Env: &Env{Key: "EMPTY", Value: ""}},
					{CopyFromImage: &CopyFromImage{
						Dst:      "/usr/local/bin/tool",
						ImageKey: "base",
						SrcPath:  "/out/tool",
					}},
				},
			},
			{
				Key:      "base",
				Platform: Platform{OS: "linux", Architecture: "x86_64"},
				Steps: []Step{{
					From: &From{Ref: "alpine:3.23"},
				}},
			},
		},
	}
}

func assertBuildError(t *testing.T, build Build, architecture string, want string) {
	t.Helper()
	err := Validate(build, architecture)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want containing %q", err, want)
	}
}
