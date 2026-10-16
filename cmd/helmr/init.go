package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/helmrdotdev/helmr/internal/version"
	"github.com/helmrdotdev/helmr/starters"
	"github.com/spf13/cobra"
)

func initCommand() *cobra.Command {
	var dir, templateID, provider, name string
	var force bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create a starter Helmr Agent project.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			root := "."
			if dir != "" {
				root = dir
			}
			if templateID == "" {
				if provider != "" || name != "" {
					return errors.New("--provider and --name need --template")
				}
				if err := writeStarterProject(root, force); err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), "created helmr.config.ts")
				fmt.Fprintln(cmd.OutOrStdout(), "created .helmrignore")
				fmt.Fprintln(cmd.OutOrStdout(), "created package.json")
				fmt.Fprintln(cmd.OutOrStdout(), "created tsconfig.json")
				fmt.Fprintln(cmd.OutOrStdout(), "created agents/hello.ts")
				return nil
			}
			return writeTemplateProject(cmd.OutOrStdout(), root, templateID, provider, name, force)
		},
	}
	cmd.Flags().StringVar(&dir, "dir", "", "Project root to initialize.")
	cmd.Flags().BoolVar(&force, "force", false, "Overwrite existing generated files.")
	cmd.Flags().StringVar(&templateID, "template", "", "Agent template: "+strings.Join(starters.IDs(), ", ")+".")
	cmd.Flags().StringVar(&provider, "provider", "", "Model provider for the template. Defaults to the template's first provider.")
	cmd.Flags().StringVar(&name, "name", "", "Agent name. Defaults to a name picked at random.")
	return cmd
}

func writeTemplateProject(out io.Writer, root, templateID, provider, name string, force bool) error {
	starter, ok := starters.Lookup(templateID)
	if !ok {
		return fmt.Errorf("unknown template %q (available: %s)", templateID, strings.Join(starters.IDs(), ", "))
	}
	binding, err := starter.Binding(provider)
	if err != nil {
		return err
	}
	if name == "" {
		name = starters.RandomName()
	} else if !starters.ValidAgentName(name) {
		return fmt.Errorf("invalid agent name %q: use lowercase letters, digits and hyphens, starting with a letter, up to 40 characters", name)
	}
	paths, err := starter.ProjectFiles(name)
	if err != nil {
		return err
	}
	pkg, err := starter.PackageJSON(starterSDKVersion(), version.Node())
	if err != nil {
		return err
	}
	if !force {
		for _, path := range paths {
			target := filepath.Join(root, filepath.FromSlash(path))
			if _, err := os.Stat(target); err == nil {
				return fmt.Errorf("%s already exists; pass --force to overwrite", target)
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	agentPath := starters.AgentPath(name)
	for _, path := range paths {
		var contents []byte
		switch path {
		case starters.PackageFile:
			contents = []byte(pkg)
		case starters.SettingsFile:
			contents = []byte(starters.Settings(name, binding))
		case agentPath:
			contents, err = fs.ReadFile(starter.Files(), starters.AgentFile)
		default:
			contents, err = fs.ReadFile(starter.Files(), path)
		}
		if err != nil {
			return err
		}
		target := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(target, contents, 0o644); err != nil {
			return err
		}
		fmt.Fprintln(out, "created "+path)
	}
	agent := "Agent: " + name
	if namesake, ok := starters.LookupNamesake(name); ok {
		agent += " (named after " + namesake.Person + ")"
	}
	fmt.Fprintln(out, agent)
	fmt.Fprintln(out, "Model: "+binding.Model+" · key from Secret "+binding.Provider.Secret+", sent only to "+binding.Provider.Origin)
	fmt.Fprintln(out, "Next: install dependencies, then helmr deploy")
	return nil
}

func writeStarterProject(root string, force bool) error {
	files := map[string]string{
		"helmr.config.ts": starterHelmrConfig,
		".helmrignore":    starterHelmrIgnore,
		"package.json":    starterPackageJSON(),
		"agents/hello.ts": starterHelloAgent,
		"tsconfig.json":   starterTSConfig,
	}
	if !force {
		for _, name := range []string{".helmrignore", "helmr.config.ts", "package.json", "agents/hello.ts", "tsconfig.json"} {
			path := filepath.Join(root, filepath.FromSlash(name))
			if _, err := os.Stat(path); err == nil {
				return fmt.Errorf("%s already exists; pass --force to overwrite", path)
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "agents"), 0o755); err != nil {
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
  dirs: ["agents"],
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
  "packageManager": "bun@1.3.13",
  "devEngines": {
    "runtime": {
      "name": "node",
      "version": ` + strconv.Quote(version.Node()) + `,
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
  "include": ["agents/**/*.ts"]
}
`

const starterHelloAgent = `import { agent, template, image } from "@helmr/sdk"

export const helloComputer = template({
  id: "hello",
  image: image("hello").from("node:24-bookworm-slim").workdir("/app"),
  resources: { cpu: 1, memory: "1GiB" },
})

export const hello = agent({
  id: "hello",
  template: helloComputer,
  async turn(turn) {
    await turn.respond("Hello from Helmr")
    return { ok: true }
  },
})
`
