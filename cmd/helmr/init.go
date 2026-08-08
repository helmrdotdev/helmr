package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/helmrdotdev/helmr/internal/version"
	"github.com/spf13/cobra"
)

func initCommand() *cobra.Command {
	var dir string
	var force bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create a starter Helmr task project.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			root := "."
			if dir != "" {
				root = dir
			}
			if err := writeStarterProject(root, force); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "created helmr.config.ts")
			fmt.Fprintln(cmd.OutOrStdout(), "created .helmrignore")
			fmt.Fprintln(cmd.OutOrStdout(), "created package.json")
			fmt.Fprintln(cmd.OutOrStdout(), "created tsconfig.json")
			fmt.Fprintln(cmd.OutOrStdout(), "created tasks/hello.ts")
			return nil
		},
	}
	cmd.Flags().StringVar(&dir, "dir", "", "Project root to initialize.")
	cmd.Flags().BoolVar(&force, "force", false, "Overwrite existing generated files.")
	return cmd
}

func writeStarterProject(root string, force bool) error {
	files := map[string]string{
		"helmr.config.ts": starterHelmrConfig,
		".helmrignore":    starterHelmrIgnore,
		"package.json":    starterPackageJSON(),
		"tasks/hello.ts":  starterHelloTask,
		"tsconfig.json":   starterTSConfig,
	}
	if !force {
		for _, name := range []string{".helmrignore", "helmr.config.ts", "package.json", "tasks/hello.ts", "tsconfig.json"} {
			path := filepath.Join(root, filepath.FromSlash(name))
			if _, err := os.Stat(path); err == nil {
				return fmt.Errorf("%s already exists; pass --force to overwrite", path)
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "tasks"), 0o755); err != nil {
		return err
	}
	for name, contents := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			return err
		}
	}
	return nil
}

const starterHelmrConfig = `import { defineConfig } from "@helmr/sdk"

export default defineConfig({
  dirs: ["tasks"],
  ignorePatterns: ["**/*.test.ts"],
})
`

const starterHelmrIgnore = `node_modules/
helmr/
dist/
.env
.env.*
!.env*.example
!.env*.sample
!.env*.template
`

func starterPackageJSON() string {
	return `{
  "private": true,
  "type": "module",
  "packageManager": "bun@1.3.10",
  "devEngines": {
    "runtime": {
      "name": "node",
      "version": "24.16.0",
      "onFail": "error"
    }
  },
  "dependencies": {
    "@helmr/sdk": ` + strconv.Quote(starterSDKVersion()) + `
  }
}
`
}

func starterSDKVersion() string {
	raw := strings.TrimPrefix(strings.TrimSpace(version.Version), "v")
	if raw == "" || raw == "dev" || strings.Contains(raw, "test") || strings.Contains(raw, "dev") || strings.Contains(raw, "dirty") || !isSemverVersion(raw) {
		return "latest"
	}
	return raw
}

func isSemverVersion(value string) bool {
	core, prerelease, hasPrerelease := strings.Cut(value, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		for _, r := range part {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	if hasPrerelease && strings.TrimSpace(prerelease) == "" {
		return false
	}
	return true
}

const starterTSConfig = `{
  "compilerOptions": {
    "target": "ES2024",
    "module": "NodeNext",
    "moduleResolution": "NodeNext",
    "noEmit": true,
    "strict": true,
    "skipLibCheck": true,
    "erasableSyntaxOnly": true,
    "verbatimModuleSyntax": true
  },
  "include": ["tasks/**/*.ts"]
}
`

const starterHelloTask = `import { image, sandbox, source, task } from "@helmr/sdk"

const runtime = image("hello")
  .from("node:24-bookworm-slim")
  .workdir("/app")
  .run(["npm", "install", "-g", "bun@1.3.10"])
  .copy("/app/package.json", source.file("package.json"))
  .run(["bun", "install"])

export const helloSandbox = sandbox({ id: "hello" })
  .image(runtime)
  .resources({ cpu: 1, memory: "1GiB" })

export const hello = task({
  id: "hello",
  run: async () => ({ ok: true }),
})
`
