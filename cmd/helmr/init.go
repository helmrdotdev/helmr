package main

import (
	"encoding/json"
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
			fmt.Fprintln(cmd.OutOrStdout(), "created or updated package.json")
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
		"tasks/hello.ts":  starterHelloTask,
	}
	if !force {
		for _, name := range []string{"helmr.config.ts", "tasks/hello.ts"} {
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
	if err := ensureStarterPackageJSON(root); err != nil {
		return err
	}
	return nil
}

const starterHelmrConfig = `import { defineConfig } from "@helmr/sdk"

export default defineConfig({
  dirs: ["./tasks"],
})
`

func starterPackageJSON() string {
	return `{
  "private": true,
  "type": "module",
  "dependencies": {
    "@helmr/sdk": ` + strconv.Quote(starterSDKVersion()) + `
  }
}
`
}

func ensureStarterPackageJSON(root string) error {
	path := filepath.Join(root, "package.json")
	current, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return os.WriteFile(path, []byte(starterPackageJSON()), 0o644)
		}
		return err
	}
	var packageJSON map[string]any
	if err := json.Unmarshal(current, &packageJSON); err != nil {
		return fmt.Errorf("decode package.json: %w", err)
	}
	dependencies, ok := packageJSON["dependencies"].(map[string]any)
	if !ok {
		dependencies = map[string]any{}
		packageJSON["dependencies"] = dependencies
	}
	if _, ok := dependencies["@helmr/sdk"]; !ok {
		dependencies["@helmr/sdk"] = starterSDKVersion()
	}
	next, err := json.MarshalIndent(packageJSON, "", "  ")
	if err != nil {
		return err
	}
	next = append(next, '\n')
	return os.WriteFile(path, next, 0o644)
}

func starterSDKVersion() string {
	raw := strings.TrimPrefix(strings.TrimSpace(version.Version), "v")
	if raw == "" || raw == "dev" || strings.Contains(raw, "test") {
		return "latest"
	}
	return raw
}

const starterHelloTask = `import { image, sandbox, task } from "@helmr/sdk"

const sb = sandbox("hello")
  .image(image("hello").from("debian:trixie-slim"))
  .workspace("/app")

export const hello = task({
  id: "hello",
  sandbox: sb,
  run: async () => ({ ok: true }),
})
`
