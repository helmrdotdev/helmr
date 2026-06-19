#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
cd "$repo_root"

version="${PACKAGE_VERSION:-}"
if [ -z "$version" ]; then
	version="$(node -p 'require("./sdk/typescript/package.json").version')"
fi

if ! printf '%s' "$version" | grep -Eq '^[0-9]+[.][0-9]+[.][0-9]+(-[0-9A-Za-z-]+([.][0-9A-Za-z-]+)*)?([+][0-9A-Za-z-]+([.][0-9A-Za-z-]+)*)?$'; then
	echo "PACKAGE_VERSION must be a semver version without a leading v: $version" >&2
	exit 1
fi

out_root="dist/npm"
proto_pkg="$out_root/proto/package"
sdk_pkg="$out_root/sdk/package"

fix_declaration_imports() {
	node - "$1" <<'NODE'
const fs = require("node:fs")
const path = require("node:path")

const root = process.argv[2]

function declarationFiles(dir) {
	const entries = fs.readdirSync(dir, { withFileTypes: true })
	const files = []
	for (const entry of entries) {
		const fullPath = path.join(dir, entry.name)
		if (entry.isDirectory()) {
			files.push(...declarationFiles(fullPath))
		} else if (entry.isFile() && entry.name.endsWith(".d.ts")) {
			files.push(fullPath)
		}
	}
	return files
}

function normalizeSpecifier(specifier) {
	if (!specifier.startsWith(".")) {
		return specifier
	}
	if (specifier === ".") {
		return "./index.js"
	}
	if (specifier === "..") {
		return "../index.js"
	}
	if (/[.]([cm]?js|json)$/.test(specifier)) {
		return specifier
	}
	return `${specifier}.js`
}

for (const file of declarationFiles(root)) {
	const original = fs.readFileSync(file, "utf8")
	const updated = original
		.replace(/(from\s+["'])(\.{1,2}(?:\/[^"']*)?)(["'])/g, (_match, before, specifier, after) => {
			return `${before}${normalizeSpecifier(specifier)}${after}`
		})
		.replace(/(import\s*\(\s*["'])(\.{1,2}(?:\/[^"']*)?)(["']\s*\))/g, (_match, before, specifier, after) => {
			return `${before}${normalizeSpecifier(specifier)}${after}`
		})
		.replace(/(import\s+["'])(\.{1,2}(?:\/[^"']*)?)(["'])/g, (_match, before, specifier, after) => {
			return `${before}${normalizeSpecifier(specifier)}${after}`
		})
	if (updated !== original) {
		fs.writeFileSync(file, updated)
	}
}
NODE
}

rm -rf "$out_root"
mkdir -p "$proto_pkg/dist" "$sdk_pkg/dist"

bun build proto/typescript/src/index.ts \
	--target node \
	--format esm \
	--packages external \
	--outfile "$proto_pkg/dist/index.js"

bun x tsc -p proto/typescript/tsconfig.build.json --outDir "$proto_pkg/dist"
fix_declaration_imports "$proto_pkg/dist"

cat > "$proto_pkg/package.json" <<EOF
{
  "name": "@helmr/proto",
  "version": "$version",
  "description": "Generated protocol bindings for Helmr.",
  "type": "module",
  "license": "Apache-2.0",
  "main": "./dist/index.js",
  "types": "./dist/index.d.ts",
  "repository": {
    "type": "git",
    "url": "git+https://github.com/helmrdotdev/helmr.git",
    "directory": "proto/typescript"
  },
  "bugs": {
    "url": "https://github.com/helmrdotdev/helmr/issues"
  },
  "homepage": "https://helmr.dev",
  "publishConfig": {
    "access": "public"
  },
  "exports": {
    ".": {
      "types": "./dist/index.d.ts",
      "import": "./dist/index.js"
    }
  },
  "files": [
    "dist",
    "LICENSE",
    "README.md"
  ],
  "dependencies": {
    "@bufbuild/protobuf": "^2.11.0"
  }
}
EOF

cp proto/README.md "$proto_pkg/README.md"
cp LICENSE "$proto_pkg/LICENSE"

bun build sdk/typescript/src/index.ts \
	--target node \
	--format esm \
	--packages external \
	--define HELMR_SDK_PACKAGE_VERSION='"'"$version"'"' \
	--outfile "$sdk_pkg/dist/index.js"
bun build sdk/typescript/src/internal.ts \
	--target node \
	--format esm \
	--packages external \
	--define HELMR_SDK_PACKAGE_VERSION='"'"$version"'"' \
	--outfile "$sdk_pkg/dist/internal.js"
bun build sdk/typescript/src/compile.ts \
	--target node \
	--format esm \
	--packages external \
	--define HELMR_SDK_PACKAGE_VERSION='"'"$version"'"' \
	--outfile "$sdk_pkg/dist/compile.js"
bun build sdk/typescript/src/fuzzy.ts \
	--target node \
	--format esm \
	--packages external \
	--define HELMR_SDK_PACKAGE_VERSION='"'"$version"'"' \
	--outfile "$sdk_pkg/dist/fuzzy.js"

bun x tsc -p sdk/typescript/tsconfig.build.json --outDir "$sdk_pkg/dist"
fix_declaration_imports "$sdk_pkg/dist"

cat > "$sdk_pkg/package.json" <<EOF
{
  "name": "@helmr/sdk",
  "version": "$version",
  "description": "TypeScript SDK for authoring and starting Helmr tasks.",
  "type": "module",
  "license": "Apache-2.0",
  "main": "./dist/index.js",
  "types": "./dist/index.d.ts",
  "repository": {
    "type": "git",
    "url": "git+https://github.com/helmrdotdev/helmr.git",
    "directory": "sdk/typescript"
  },
  "bugs": {
    "url": "https://github.com/helmrdotdev/helmr/issues"
  },
  "homepage": "https://helmr.dev",
  "publishConfig": {
    "access": "public"
  },
  "exports": {
    ".": {
      "types": "./dist/index.d.ts",
      "import": "./dist/index.js"
    },
    "./internal": {
      "types": "./dist/internal.d.ts",
      "import": "./dist/internal.js"
    },
    "./internal/compile": {
      "types": "./dist/compile.d.ts",
      "import": "./dist/compile.js"
    },
    "./internal/fuzzy": {
      "types": "./dist/fuzzy.d.ts",
      "import": "./dist/fuzzy.js"
    }
  },
  "files": [
    "dist",
    "LICENSE",
    "README.md"
  ],
  "dependencies": {
    "@bufbuild/protobuf": "^2.11.0",
    "@helmr/proto": "$version"
  }
}
EOF

cp sdk/README.md "$sdk_pkg/README.md"
cp LICENSE "$sdk_pkg/LICENSE"
