package deployment

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/archive"
)

const (
	npmSourcePackageJSON  = `{"devEngines":{"runtime":{"name":"node","version":"24.16.0"}},"packageManager":"npm@11.4.2","type":"module"}`
	pnpmSourcePackageJSON = `{"devEngines":{"runtime":{"name":"node","version":"24.16.0"}},"packageManager":"pnpm@11.1.0","type":"module"}`
	bunSourcePackageJSON  = `{"devEngines":{"runtime":{"name":"node","version":"24.16.0"}},"packageManager":"bun@1.3.11","type":"module"}`
	bunSourceLock         = `{"configVersion":1,"lockfileVersion":1,"packages":{},"workspaces":{"":{"name":"source-fixture"}}}`
)

func TestInspectSourcePinsExactManagerAndLockfile(t *testing.T) {
	tests := []struct {
		name     string
		files    map[string]string
		manager  PackageManager
		lockfile string
	}{
		{
			name: "npm",
			files: map[string]string{
				"package.json":      npmSourcePackageJSON,
				"package-lock.json": `{"lockfileVersion":3}`,
			},
			manager:  PackageManager{Name: PackageManagerNPM, Version: "11.4.2"},
			lockfile: "package-lock.json",
		},
		{
			name: "npm shrinkwrap",
			files: map[string]string{
				"package.json":        npmSourcePackageJSON,
				"npm-shrinkwrap.json": `{"lockfileVersion":3}`,
			},
			manager:  PackageManager{Name: PackageManagerNPM, Version: "11.4.2"},
			lockfile: "npm-shrinkwrap.json",
		},
		{
			name: "pnpm",
			files: map[string]string{
				"package.json":   pnpmSourcePackageJSON,
				"pnpm-lock.yaml": "lockfileVersion: '9.0'",
			},
			manager:  PackageManager{Name: PackageManagerPNPM, Version: "11.1.0"},
			lockfile: "pnpm-lock.yaml",
		},
		{
			name: "bun",
			files: map[string]string{
				"package.json": bunSourcePackageJSON,
				"bun.lock":     bunSourceLock,
			},
			manager:  PackageManager{Name: PackageManagerBun, Version: "1.3.11"},
			lockfile: "bun.lock",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			selection, err := InspectSource(sourceTar(t, test.files, nil))
			if err != nil {
				t.Fatal(err)
			}
			if selection.Manager != test.manager || selection.LockfileName != test.lockfile {
				t.Fatalf("selection = %#v", selection)
			}
			if selection.LockfileDigest != digestBytes([]byte(test.files[test.lockfile])) {
				t.Fatalf("lockfile digest = %q", selection.LockfileDigest)
			}
		})
	}
}

func TestInspectSourceAcceptsCanonicalCLIArtifact(t *testing.T) {
	root := t.TempDir()
	for name, body := range map[string]string{
		".helmrignore":      "*.secret\n",
		"helmr.config.ts":   `export default { dirs: ["tasks"] }`,
		"ignored.secret":    "not submitted",
		"package.json":      npmSourcePackageJSON,
		"package-lock.json": `{"lockfileVersion":3}`,
		"src/task.ts":       "export {}",
	} {
		filename := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	artifact, cleanup, err := archive.CreateTarWithOptions(root, t.TempDir(), archive.TarOptions{
		CanonicalSource: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	file, err := os.Open(artifact.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	selection, err := InspectSource(file)
	if err != nil {
		t.Fatal(err)
	}
	if selection.Manager != (PackageManager{Name: PackageManagerNPM, Version: "11.4.2"}) {
		t.Fatalf("selection manager = %#v", selection.Manager)
	}
}

func TestInspectSourcePreservesCorepackManagerIntegrity(t *testing.T) {
	integrity := "sha256." + strings.Repeat("a", 64)
	files := map[string]string{
		"package.json":   `{"devEngines":{"runtime":{"name":"node","version":"24.16.0"}},"packageManager":"pnpm@11.1.0+` + integrity + `","type":"module"}`,
		"pnpm-lock.yaml": "lockfileVersion: '9.0'",
	}
	selection, err := InspectSource(sourceTar(t, files, nil))
	if err != nil {
		t.Fatal(err)
	}
	if selection.Manager != (PackageManager{
		Integrity: integrity,
		Name:      PackageManagerPNPM,
		Version:   "11.1.0",
	}) {
		t.Fatalf("selection manager = %#v", selection.Manager)
	}
}

func TestInspectSourceRejectsAmbiguousOrInvalidAuthority(t *testing.T) {
	tests := map[string]map[string]string{
		"missing package manager": {
			"package.json": `{}`,
			"bun.lock":     bunSourceLock,
		},
		"manager range": {
			"package.json": `{"devEngines":{"runtime":{"name":"node","version":"24.16.0"}},"packageManager":"bun@^1.3.11","type":"module"}`,
			"bun.lock":     bunSourceLock,
		},
		"invalid manager integrity": {
			"package.json":      `{"devEngines":{"runtime":{"name":"node","version":"24.16.0"}},"packageManager":"npm@11.4.2+sha256.not-hex","type":"module"}`,
			"package-lock.json": `{"lockfileVersion":3}`,
		},
		"bun integrity suffix": {
			"package.json": `{"devEngines":{"runtime":{"name":"node","version":"24.16.0"}},"packageManager":"bun@1.3.11+sha256.` + strings.Repeat("a", 64) + `","type":"module"}`,
			"bun.lock":     bunSourceLock,
		},
		"missing selected lockfile": {
			"package.json": npmSourcePackageJSON,
			"bun.lock":     bunSourceLock,
		},
		"reserved output": {
			"package.json": bunSourcePackageJSON,
			"bun.lock":     bunSourceLock,
			"helmr/output": "reserved",
		},
		"nested reserved output": {
			"package.json":              bunSourcePackageJSON,
			"bun.lock":                  bunSourceLock,
			"packages/tool/.helmr/file": "reserved",
		},
	}
	for name, files := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := InspectSource(sourceTar(t, files, nil)); err == nil {
				t.Fatal("InspectSource returned nil error")
			}
		})
	}
}

func TestInspectSourceAllowsNestedReservedNames(t *testing.T) {
	files := map[string]string{
		"package.json":                 bunSourcePackageJSON,
		"bun.lock":                     bunSourceLock,
		"packages/tool/helmr/value.ts": "export {}",
		"packages/tool/node_modules/x": "x",
	}
	if _, err := InspectSource(sourceTar(t, files, nil)); err != nil {
		t.Fatal(err)
	}
}

func TestInspectSourceRejectsDuplicatePathsAndEscapingLinks(t *testing.T) {
	duplicate := sourceTar(t, map[string]string{
		"package.json": bunSourcePackageJSON,
		"bun.lock":     bunSourceLock,
	}, []tar.Header{{Name: "bun.lock", Typeflag: tar.TypeReg, Size: int64(len(bunSourceLock)), Mode: 0o644}})
	if _, err := InspectSource(duplicate); err == nil {
		t.Fatal("InspectSource accepted a duplicate path")
	}

	escaping := sourceTar(t, map[string]string{
		"package.json": bunSourcePackageJSON,
		"bun.lock":     bunSourceLock,
	}, []tar.Header{{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "../../outside"}})
	if _, err := InspectSource(escaping); err == nil {
		t.Fatal("InspectSource accepted an escaping link")
	}
}

func TestInspectSourceRejectsMissingParentAndRootGit(t *testing.T) {
	tests := map[string][]tar.Header{
		"missing parent": {
			{Name: "nested/task.ts", Typeflag: tar.TypeReg, Size: 1, Mode: 0o644},
		},
		"root git": {
			{Name: ".git", Typeflag: tar.TypeReg, Size: 1, Mode: 0o644},
		},
	}
	for name, extra := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := InspectSource(sourceTar(t, map[string]string{
				"package.json":      npmSourcePackageJSON,
				"package-lock.json": `{"lockfileVersion":3}`,
			}, extra)); err == nil {
				t.Fatal("InspectSource accepted a noncanonical source tree")
			}
		})
	}
}

func TestInspectSourceRevalidatesHelmrIgnore(t *testing.T) {
	if _, err := InspectSource(sourceTar(t, map[string]string{
		".helmrignore":       "*.secret\n",
		"credentials.secret": "included",
		"package.json":       npmSourcePackageJSON,
		"package-lock.json":  `{"lockfileVersion":3}`,
	}, nil)); err == nil {
		t.Fatal("InspectSource accepted bytes excluded by .helmrignore")
	}
}

func TestInspectSourceRejectsEnvironmentSecretsAndAcceptsExamples(t *testing.T) {
	for _, name := range []string{".env", ".env.local", "packages/api/.env.production"} {
		t.Run("reject "+name, func(t *testing.T) {
			if _, err := InspectSource(sourceTar(t, map[string]string{
				name:                "TOKEN=secret",
				"package.json":      npmSourcePackageJSON,
				"package-lock.json": `{"lockfileVersion":3}`,
			}, nil)); err == nil || !strings.Contains(err.Error(), "likely secret") {
				t.Fatalf("InspectSource error = %v", err)
			}
		})
	}
	for _, name := range []string{".env.example", ".env.production.sample", "packages/api/.env.template"} {
		t.Run("accept "+name, func(t *testing.T) {
			if _, err := InspectSource(sourceTar(t, map[string]string{
				name:                "TOKEN=",
				"package.json":      npmSourcePackageJSON,
				"package-lock.json": `{"lockfileVersion":3}`,
			}, nil)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestInspectSourceAcceptsSafeManagerConfiguration(t *testing.T) {
	tests := map[string]map[string]string{
		"npm": {
			".npmrc":            "registry=https://registry.example.com/npm/\nlegacy-peer-deps=true\n",
			"package.json":      npmSourcePackageJSON,
			"package-lock.json": `{"lockfileVersion":3}`,
		},
		"pnpm": {
			".npmrc":              "registry=https://registry.example.com/npm/\nalways-auth=false\n",
			"package.json":        pnpmSourcePackageJSON,
			"pnpm-lock.yaml":      "lockfileVersion: '9.0'\n",
			"pnpm-workspace.yaml": "packages:\n  - packages/*\nregistries:\n  default: https://registry.example.com/npm/\nnamedRegistries:\n  public: https://packages.example.com/npm/\nstrictSsl: true\n",
		},
		"bun": {
			".npmrc":       "@public:registry=https://registry.example.com/npm/\n",
			"bun.lock":     bunSourceLock,
			"bunfig.toml":  "[install]\nexact = true\nregistry = \"https://registry.example.com/npm/\"\n[install.scopes]\npublic = { url = \"https://packages.example.com/npm/\" }\n",
			"package.json": bunSourcePackageJSON,
		},
		"pnpm build hook": {
			".pnpmfile.cjs":  "module.exports = { hooks: {} }",
			"package.json":   pnpmSourcePackageJSON,
			"pnpm-lock.yaml": "lockfileVersion: '9.0'",
		},
	}
	for name, files := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := InspectSource(sourceTar(t, files, nil)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestInspectSourceRejectsManagerConfigurationAuthority(t *testing.T) {
	tests := map[string]map[string]string{
		"npm token": {
			".npmrc":            "//registry.example.com/:_authToken=secret\n",
			"package.json":      npmSourcePackageJSON,
			"package-lock.json": `{"lockfileVersion":3}`,
		},
		"npm interpolated token": {
			".npmrc":            "//registry.example.com/:_authToken=${NPM_TOKEN}\n",
			"package.json":      npmSourcePackageJSON,
			"package-lock.json": `{"lockfileVersion":3}`,
		},
		"npm registry userinfo": {
			".npmrc":            "registry=https://user:password@registry.example.com/\n",
			"package.json":      npmSourcePackageJSON,
			"package-lock.json": `{"lockfileVersion":3}`,
		},
		"npm plain HTTP": {
			".npmrc":            "registry=http://registry.example.com/\n",
			"package.json":      npmSourcePackageJSON,
			"package-lock.json": `{"lockfileVersion":3}`,
		},
		"npm proxy": {
			".npmrc":            "https-proxy=https://proxy.example.com/\n",
			"package.json":      npmSourcePackageJSON,
			"package-lock.json": `{"lockfileVersion":3}`,
		},
		"npm custom trust": {
			".npmrc":            "cafile=certificates/ca.pem\n",
			"package.json":      npmSourcePackageJSON,
			"package-lock.json": `{"lockfileVersion":3}`,
		},
		"npm TLS weakening": {
			".npmrc":            "strict-ssl=false\n",
			"package.json":      npmSourcePackageJSON,
			"package-lock.json": `{"lockfileVersion":3}`,
		},
		"pnpm registry interpolation": {
			"package.json":        pnpmSourcePackageJSON,
			"pnpm-lock.yaml":      "lockfileVersion: '9.0'",
			"pnpm-workspace.yaml": "registries:\n  default: https://${REGISTRY_HOST}/\n",
		},
		"pnpm proxy": {
			"package.json":        pnpmSourcePackageJSON,
			"pnpm-lock.yaml":      "lockfileVersion: '9.0'",
			"pnpm-workspace.yaml": "httpsProxy: https://proxy.example.com/\n",
		},
		"pnpm TLS weakening": {
			"package.json":        pnpmSourcePackageJSON,
			"pnpm-lock.yaml":      "lockfileVersion: '9.0'",
			"pnpm-workspace.yaml": "strictSsl: false\n",
		},
		"bun token": {
			"bun.lock":     bunSourceLock,
			"bunfig.toml":  "[install]\nregistry = { url = \"https://registry.example.com/\", token = \"$NPM_TOKEN\" }\n",
			"package.json": bunSourcePackageJSON,
		},
		"bun scope credential": {
			"bun.lock":     bunSourceLock,
			"bunfig.toml":  "[install.scopes]\nprivate = { url = \"https://registry.example.com/\", username = \"user\", password = \"password\" }\n",
			"package.json": bunSourcePackageJSON,
		},
		"bun custom trust": {
			"bun.lock":     bunSourceLock,
			"bunfig.toml":  "[install]\ncafile = \"certificates/ca.pem\"\n",
			"package.json": bunSourcePackageJSON,
		},
		"bun Fetch scanner": {
			"bun.lock":     bunSourceLock,
			"bunfig.toml":  "[install.security]\nscanner = \"@example/scanner\"\n",
			"package.json": bunSourcePackageJSON,
		},
	}
	for name, files := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := InspectSource(sourceTar(t, files, nil)); err == nil {
				t.Fatal("InspectSource accepted Manager configuration authority")
			}
		})
	}
}

func TestInspectSourceRejectsNonUSTARPath(t *testing.T) {
	files := map[string]string{
		"package.json":           npmSourcePackageJSON,
		"package-lock.json":      `{"lockfileVersion":3}`,
		strings.Repeat("x", 101): "pax",
	}
	if _, err := InspectSource(sourceTar(t, files, nil)); err == nil {
		t.Fatal("InspectSource accepted a non-USTAR source path")
	}
}

func TestInspectSourceRejectsNoncanonicalUSTARBytes(t *testing.T) {
	valid := sourceTar(t, map[string]string{
		"package-lock.json": `{"lockfileVersion":3}`,
		"package.json":      npmSourcePackageJSON,
	}, nil)
	validBytes, err := io.ReadAll(valid)
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string][]byte{
		"trailing bytes": append(append([]byte(nil), validBytes...), 1),
		"nonzero padding": func() []byte {
			raw := append([]byte(nil), validBytes...)
			firstSize := len(`export default { dirs: ["tasks"] }`)
			raw[tarBlockSize+firstSize] = 1
			return raw
		}(),
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := InspectSource(bytes.NewReader(raw)); err == nil {
				t.Fatal("InspectSource accepted noncanonical source bytes")
			}
		})
	}

	var body bytes.Buffer
	writer := tar.NewWriter(&body)
	if err := writer.WriteHeader(&tar.Header{
		Name:     "package-lock.json",
		Typeflag: tar.TypeReg,
		Mode:     0o600,
		Size:     int64(len(`{"lockfileVersion":3}`)),
		Format:   tar.FormatUSTAR,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte(`{"lockfileVersion":3}`)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectSource(bytes.NewReader(body.Bytes())); err == nil {
		t.Fatal("InspectSource accepted a noncanonical regular-file mode")
	}
}

func TestInspectSourceRejectsRootHeaderAndDirectorySortDrift(t *testing.T) {
	for name, headers := range map[string][]tar.Header{
		"root header": {
			{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755, Format: tar.FormatUSTAR},
		},
		"directory order": {
			{Name: "a/", Typeflag: tar.TypeDir, Mode: 0o755, Format: tar.FormatUSTAR},
			{Name: "a.", Typeflag: tar.TypeReg, Mode: 0o644, Format: tar.FormatUSTAR},
		},
	} {
		t.Run(name, func(t *testing.T) {
			var body bytes.Buffer
			writer := tar.NewWriter(&body)
			for index := range headers {
				if err := writer.WriteHeader(&headers[index]); err != nil {
					t.Fatal(err)
				}
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := InspectSource(bytes.NewReader(body.Bytes())); err == nil {
				t.Fatal("InspectSource accepted noncanonical path encoding/order")
			}
		})
	}
}

func TestUSTARPathRepresentable(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{name: strings.Repeat("x", 100), want: true},
		{name: strings.Repeat("x", 101), want: false},
		{
			name: strings.Repeat("p", 155) + "/" + strings.Repeat("n", 100),
			want: true,
		},
		{
			name: strings.Repeat("p", 156) + "/" + strings.Repeat("n", 100),
			want: false,
		},
		{
			name: strings.Repeat("a", 100) + "/" + strings.Repeat("b", 55) + "/c",
			want: false,
		},
		{name: strings.Repeat("d", 99) + "/", want: true},
		{name: strings.Repeat("d", 100) + "/", want: false},
	}
	for _, test := range tests {
		if got := ustarPathRepresentable(test.name); got != test.want {
			t.Fatalf(
				"ustarPathRepresentable(%q) = %v, want %v",
				test.name,
				got,
				test.want,
			)
		}
	}
}

func sourceTar(t *testing.T, files map[string]string, extra []tar.Header) *bytes.Reader {
	t.Helper()
	if _, exists := files["helmr.config.ts"]; !exists {
		copied := make(map[string]string, len(files)+1)
		for name, body := range files {
			copied[name] = body
		}
		copied["helmr.config.ts"] = `export default { dirs: ["tasks"] }`
		files = copied
	}
	var body bytes.Buffer
	writer := tar.NewWriter(&body)
	directories := make(map[string]struct{})
	for name := range files {
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			directories[parent] = struct{}{}
		}
	}
	names := make([]string, 0, len(files)+len(directories))
	for name := range directories {
		names = append(names, name+"/")
	}
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if strings.HasSuffix(name, "/") {
			if err := writer.WriteHeader(&tar.Header{
				Name:     name,
				Typeflag: tar.TypeDir,
				Mode:     0o755,
				Format:   tar.FormatUSTAR,
			}); err != nil {
				t.Fatal(err)
			}
			continue
		}
		value := files[name]
		header := &tar.Header{
			Name:     name,
			Typeflag: tar.TypeReg,
			Mode:     0o644,
			Size:     int64(len(value)),
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte(value)); err != nil {
			t.Fatal(err)
		}
	}
	for _, value := range extra {
		header := value
		if err := writer.WriteHeader(&header); err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeReg && header.Size > 0 {
			if _, err := writer.Write([]byte(strings.Repeat("x", int(header.Size)))); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(body.Bytes())
}
