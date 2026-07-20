package builder

import (
	"strings"
	"testing"
)

func TestValidateImageBuild(t *testing.T) {
	build := validImageBuild()
	if err := ValidateImageBuild(build, "x86_64"); err != nil {
		t.Fatalf("ValidateImageBuild: %v", err)
	}
	if got := ImageBuildStepCount(build); got != 10 {
		t.Fatalf("ImageBuildStepCount = %d, want 10", got)
	}
}

func TestValidateImageBuildRejectsInvalidShape(t *testing.T) {
	tests := []struct {
		name   string
		change func(*ImageBuild)
		errMsg string
	}{
		{
			name: "format version",
			change: func(build *ImageBuild) {
				build.FormatVersion = 1
			},
			errMsg: "formatVersion",
		},
		{
			name: "root",
			change: func(build *ImageBuild) {
				build.Root = "missing"
			},
			errMsg: "does not name an image",
		},
		{
			name: "image order",
			change: func(build *ImageBuild) {
				build.Images[0], build.Images[1] = build.Images[1], build.Images[0]
			},
			errMsg: "sorted by key",
		},
		{
			name: "platform",
			change: func(build *ImageBuild) {
				build.Images[0].Platform.OS = "darwin"
			},
			errMsg: "platform.os",
		},
		{
			name: "architecture",
			change: func(build *ImageBuild) {
				build.Images[0].Platform.Architecture = "aarch64"
			},
			errMsg: "architecture",
		},
		{
			name: "first step",
			change: func(build *ImageBuild) {
				build.Images[0].Steps = build.Images[0].Steps[1:]
			},
			errMsg: "first step must be from",
		},
		{
			name: "second from",
			change: func(build *ImageBuild) {
				build.Images[0].Steps[1] = ImageStep{
					From: &ImageFrom{Ref: "alpine:3.23"},
				}
			},
			errMsg: "more than one from",
		},
		{
			name: "empty operation",
			change: func(build *ImageBuild) {
				build.Images[0].Steps[1] = ImageStep{}
			},
			errMsg: "exactly one operation",
		},
		{
			name: "multiple operations",
			change: func(build *ImageBuild) {
				build.Images[0].Steps[1].Env = &ImageEnv{Key: "A", Value: "b"}
			},
			errMsg: "exactly one operation",
		},
		{
			name: "unknown image",
			change: func(build *ImageBuild) {
				build.Images[0].Steps[8].CopyFromImage.ImageKey = "missing"
			},
			errMsg: "unknown image",
		},
		{
			name: "cycle",
			change: func(build *ImageBuild) {
				build.Images[1].Steps = append(build.Images[1].Steps, ImageStep{
					CopyFromImage: &ImageCopyFromImage{
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
			change: func(build *ImageBuild) {
				build.Images = append(build.Images, ImageSpec{
					Key:      "unused",
					Platform: ImagePlatform{OS: "linux", Architecture: "x86_64"},
					Steps: []ImageStep{{
						From: &ImageFrom{Ref: "alpine:3.23"},
					}},
				})
			},
			errMsg: "unreachable",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			build := validImageBuild()
			test.change(&build)
			assertImageBuildError(t, build, "x86_64", test.errMsg)
		})
	}
}

func TestValidateImageBuildRejectsInvalidOperations(t *testing.T) {
	tests := []struct {
		name   string
		change func(*ImageBuild)
		errMsg string
	}{
		{
			name: "reference",
			change: func(build *ImageBuild) {
				build.Images[0].Steps[0].From.Ref = " Invalid "
			},
			errMsg: "from.ref",
		},
		{
			name: "empty argv",
			change: func(build *ImageBuild) {
				build.Images[0].Steps[1].Run.Argv = []string{}
			},
			errMsg: "argv count",
		},
		{
			name: "empty executable",
			change: func(build *ImageBuild) {
				build.Images[0].Steps[1].Run.Argv[0] = ""
			},
			errMsg: "argv[0]",
		},
		{
			name: "nil cache mounts",
			change: func(build *ImageBuild) {
				build.Images[0].Steps[1].Run.CacheMounts = nil
			},
			errMsg: "cacheMounts must be an array",
		},
		{
			name: "duplicate mount destination",
			change: func(build *ImageBuild) {
				build.Images[0].Steps[1].Run.SecretMounts[0].Dst = "/cache"
			},
			errMsg: "duplicate mount destination",
		},
		{
			name: "cache sharing",
			change: func(build *ImageBuild) {
				build.Images[0].Steps[1].Run.CacheMounts[0].Sharing = "exclusive"
			},
			errMsg: "sharing is unsupported",
		},
		{
			name: "secret name",
			change: func(build *ImageBuild) {
				build.Images[0].Steps[1].Run.SecretMounts[0].Name = "../TOKEN"
			},
			errMsg: "name is invalid",
		},
		{
			name: "destination path",
			change: func(build *ImageBuild) {
				build.Images[0].Steps[2].CopySourceFile.Dst = "app"
			},
			errMsg: "absolute POSIX path",
		},
		{
			name: "source path",
			change: func(build *ImageBuild) {
				build.Images[0].Steps[2].CopySourceFile.Path = "../package.json"
			},
			errMsg: "Deployment-relative",
		},
		{
			name: "directory dot path",
			change: func(build *ImageBuild) {
				build.Images[0].Steps[3].CopySourceDir.Path = "."
			},
		},
		{
			name: "file dot path",
			change: func(build *ImageBuild) {
				build.Images[0].Steps[2].CopySourceFile.Path = "."
			},
			errMsg: "Deployment-relative",
		},
		{
			name: "workdir parent",
			change: func(build *ImageBuild) {
				build.Images[0].Steps[4].Workdir.Path = "../app"
			},
			errMsg: "clean POSIX path",
		},
		{
			name: "user",
			change: func(build *ImageBuild) {
				build.Images[0].Steps[5].User.Name = "root:root:root"
			},
			errMsg: "OCI user",
		},
		{
			name: "environment key",
			change: func(build *ImageBuild) {
				build.Images[0].Steps[6].Env.Key = "1INVALID"
			},
			errMsg: "env.key",
		},
		{
			name: "environment aggregate",
			change: func(build *ImageBuild) {
				build.Images[0].Steps[6].Env.Value = strings.Repeat("x", maxImageEnvValueBytes)
			},
			errMsg: "environment exceeds",
		},
		{
			name: "source image path",
			change: func(build *ImageBuild) {
				build.Images[0].Steps[8].CopyFromImage.SrcPath = "/out/../secret"
			},
			errMsg: "absolute POSIX path",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			build := validImageBuild()
			test.change(&build)
			err := ValidateImageBuild(build, "x86_64")
			if test.errMsg == "" {
				if err != nil {
					t.Fatalf("ValidateImageBuild: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.errMsg) {
				t.Fatalf("error = %v, want containing %q", err, test.errMsg)
			}
		})
	}
}

func TestValidateImageBuildEnforcesIndependentBounds(t *testing.T) {
	t.Run("image key", func(t *testing.T) {
		build := validImageBuild()
		build.Root = strings.Repeat("a", maxImageIdentifierBytes+1)
		assertImageBuildError(t, build, "x86_64", "root")
	})
	t.Run("argument", func(t *testing.T) {
		build := validImageBuild()
		build.Images[0].Steps[1].Run.Argv = []string{strings.Repeat("a", maxImageArgumentBytes+1)}
		assertImageBuildError(t, build, "x86_64", "argv[0]")
	})
	t.Run("mount count", func(t *testing.T) {
		build := validImageBuild()
		build.Images[0].Steps[1].Run.CacheMounts = make([]ImageCacheMount, maxImageMounts+1)
		assertImageBuildError(t, build, "x86_64", "cache mounts")
	})
	t.Run("step count", func(t *testing.T) {
		build := validImageBuild()
		steps := make([]ImageStep, maxImageBuildSteps+1)
		steps[0] = ImageStep{From: &ImageFrom{Ref: "alpine:3.23"}}
		for index := 1; index < len(steps); index++ {
			steps[index] = ImageStep{Workdir: &ImageWorkdir{Path: "."}}
		}
		build.Images = []ImageSpec{{
			Key:      "app",
			Platform: ImagePlatform{OS: "linux", Architecture: "x86_64"},
			Steps:    steps,
		}}
		assertImageBuildError(t, build, "x86_64", "more than 10000 steps")
	})
}

func validImageBuild() ImageBuild {
	return ImageBuild{
		FormatVersion: ImageBuildFormatVersion,
		Root:          "app",
		Images: []ImageSpec{
			{
				Key:      "app",
				Platform: ImagePlatform{OS: "linux", Architecture: "x86_64"},
				Steps: []ImageStep{
					{From: &ImageFrom{Ref: "debian:bookworm-slim"}},
					{Run: &ImageRun{
						Argv: []string{"sh", "-c", "mkdir -p /app /out"},
						CacheMounts: []ImageCacheMount{{
							Dst:     "/cache",
							CacheID: "pnpm",
							Sharing: "locked",
						}},
						SecretMounts: []ImageSecretMount{{
							Dst:  "/run/secrets/TOKEN",
							Name: "TOKEN",
						}},
					}},
					{CopySourceFile: &ImageCopySourceFile{
						Dst:  "/app/package.json",
						Path: "package.json",
					}},
					{CopySourceDir: &ImageCopySourceDir{
						Dst:  "/app/src",
						Path: "src",
					}},
					{Workdir: &ImageWorkdir{Path: "/app"}},
					{User: &ImageUser{Name: "1000:1000"}},
					{Env: &ImageEnv{Key: "NODE_ENV", Value: "production"}},
					{Env: &ImageEnv{Key: "EMPTY", Value: ""}},
					{CopyFromImage: &ImageCopyFromImage{
						Dst:      "/usr/local/bin/tool",
						ImageKey: "base",
						SrcPath:  "/out/tool",
					}},
				},
			},
			{
				Key:      "base",
				Platform: ImagePlatform{OS: "linux", Architecture: "x86_64"},
				Steps: []ImageStep{{
					From: &ImageFrom{Ref: "alpine:3.23"},
				}},
			},
		},
	}
}

func assertImageBuildError(t *testing.T, build ImageBuild, architecture string, want string) {
	t.Helper()
	err := ValidateImageBuild(build, architecture)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want containing %q", err, want)
	}
}
