{
  system,
  nixpkgs,
  helmrPackages,
}:

let
  pkgs = import nixpkgs { inherit system; };
  inherit (pkgs) lib;

  commandCheck =
    name: command:
    pkgs.runCommand name
      {
        nativeBuildInputs = [
          pkgs.go_1_26
          pkgs.git
        ];
        src = ../.;
      }
      ''
        cp -R "$src" source
        chmod -R u+w source
        cd source
        export HOME="$TMPDIR/home"
        mkdir -p "$HOME"
        ${command}
        touch "$out"
      '';

  firecrackerHostEval = import (nixpkgs + "/nixos/lib/eval-config.nix") {
    inherit system;
    modules = [
      ./modules/nixos/firecracker-host.nix
      (
        { ... }:
        {
          system.stateVersion = "25.11";
          boot.loader.grub.enable = false;
          fileSystems."/" = {
            device = "none";
            fsType = "tmpfs";
          };
          users.users.helmr-ci = {
            isNormalUser = true;
          };
          services.helmr.firecrackerHost = {
            enable = true;
            users = [ "helmr-ci" ];
          };
        }
      )
    ];
  };

  require = condition: message: if condition then true else throw message;

  checkedFirecrackerHostModule =
    let
      cfg = firecrackerHostEval.config;
      buildkitService = cfg.systemd.services.helmr-buildkit.serviceConfig;
      buildkitExecStart = buildkitService.ExecStart;
      workerGroups = cfg.users.users.helmr-ci.extraGroups;
    in
    require (buildkitService.User == "helmr-buildkit") "helmr-buildkit service user changed"
    && require (buildkitService.Group == "helmr-buildkit") "helmr-buildkit service group changed"
    && require (buildkitService.Delegate == true) "helmr-buildkit service delegation changed"
    && require (buildkitService.CPUQuota == "100%") "helmr-buildkit CPU limit changed"
    && require (buildkitService.MemoryMax == "2G") "helmr-buildkit memory limit changed"
    && require (buildkitService.MemorySwapMax == 0) "helmr-buildkit swap limit changed"
    && require (buildkitService.TasksMax == 1024) "helmr-buildkit task limit changed"
    && require (buildkitService.MemoryOOMGroup == true) "helmr-buildkit OOM group policy changed"
    && require (cfg.boot.kernel.sysctl."net.ipv4.ip_forward" == 1) "IPv4 forwarding is not enabled"
    && require (
      cfg.boot.kernel.sysctl."user.max_user_namespaces" == 16384
    ) "user namespace limit changed"
    && require (lib.elem "kvm" cfg.boot.kernelModules) "kvm kernel module is not requested"
    && require (lib.elem "kvm" workerGroups) "firecracker users are not added to kvm"
    && require (lib.elem "helmr-buildkit" workerGroups) "firecracker users are not added to helmr-buildkit"
    && require (lib.hasInfix ''KERNEL=="kvm", GROUP="helmr-vmm", MODE="0660"'' cfg.services.udev.extraRules) "kvm udev rule changed"
    && require (lib.hasInfix "rootlesskit" buildkitExecStart) "BuildKit service no longer starts through rootlesskit"
    && require (lib.hasInfix "--net=slirp4netns" buildkitExecStart) "BuildKit service no longer uses slirp4netns"
    && require (lib.hasInfix "buildkitd" buildkitExecStart) "BuildKit service no longer starts buildkitd"
    && require (lib.hasInfix "unix:///run/helmr/buildkit/buildkitd.sock" buildkitExecStart) "BuildKit socket path changed";

  firecrackerHostModuleCheck =
    assert checkedFirecrackerHostModule;
    pkgs.runCommand "firecracker-host-module-check" { } ''
      touch "$out"
    '';

  preloadCheck =
    pkgs.runCommand "managed-runtime-preload-check"
      {
        nativeBuildInputs = [ pkgs.nodejs_24 ];
        src = ../.;
      }
      ''
        node --test "$src/runtime/typescript/src/preload.test.mjs"
        touch "$out"
      '';

in
{
  helmr-package = helmrPackages.helmr;
  helmr-smoke = pkgs.runCommand "helmr-smoke" { } ''
    export HOME="$TMPDIR/home"
    export XDG_CACHE_HOME="$TMPDIR/cache"
    mkdir -p "$HOME" "$XDG_CACHE_HOME"

    ${helmrPackages.helmr}/bin/helmr --version
    ${helmrPackages.helmr}/bin/helmr init --dir "$TMPDIR/project"
    test -f "$TMPDIR/project/helmr.config.ts"
    test -f "$TMPDIR/project/package.json"

    touch "$out"
  '';
  fmt = commandCheck "fmt-check" ''
    unformatted="$(find . -name '*.go' -not -path './.git/*' -print | xargs gofmt -l)"
    if [ -n "$unformatted" ]; then
      printf '%s\n' "$unformatted" >&2
      exit 1
    fi
  '';
  managed-runtime-preload = preloadCheck;
  runtime-trusted-root-contract = commandCheck "runtime-trusted-root-contract-check" ''
    cat >internal/deployment/runtime_trusted_root_release_test.go <<'EOF'
    package deployment

    import (
      "os"
      "testing"
    )

    func TestReleaseTrustedRoot(t *testing.T) {
      raw, err := os.ReadFile(os.Getenv("HELMR_RUNTIME_TRUSTED_ROOT"))
      if err != nil {
        t.Fatal(err)
      }
      if _, err := parseRuntimeTrustedRoot(raw); err != nil {
        t.Fatal(err)
      }
    }
    EOF
    gofmt -w internal/deployment/runtime_trusted_root_release_test.go
    cp -R ${helmrPackages.helmr.goModules} vendor
    export GOFLAGS=-mod=vendor
    export GOPROXY=off
    export GOSUMDB=off
    export GOTOOLCHAIN=local
    HELMR_RUNTIME_TRUSTED_ROOT=${helmrPackages.runtimeTrustedRoot} \
      go test ./internal/deployment -run '^TestReleaseTrustedRoot$'
  '';
  squashfs-tools = helmrPackages.squashfsTools;
}
// lib.optionalAttrs pkgs.stdenv.isLinux {
  firecracker-host-module = firecrackerHostModuleCheck;
  program-archive-contract =
    pkgs.runCommand "program-archive-contract-check"
      {
        nativeBuildInputs = [
          pkgs.go_1_26
          helmrPackages.squashfsTools
        ];
        src = ../.;
      }
      ''
        cp -R "$src" source
        chmod -R u+w source
        cd source
        export HOME="$TMPDIR/home"
        mkdir -p "$HOME"
        cp -R ${helmrPackages.helmr.goModules} vendor
        export GOFLAGS=-mod=vendor
        export GOPROXY=off
        export GOSUMDB=off
        export GOTOOLCHAIN=local
        export CGO_ENABLED=0
        HELMR_SQUASHFS_ENCODER=${helmrPackages.squashfsTools}/bin/mksquashfs \
          go test ./internal/deployment -run '^TestPinnedProgramEncoder$'
        touch "$out"
      '';
  managed-runtime = helmrPackages.managedRuntime;
  manager-loader-contract = commandCheck "manager-loader-contract-check" ''
    cp -R ${helmrPackages.helmr.goModules} vendor
    export GOFLAGS=-mod=vendor
    export GOPROXY=off
    export GOSUMDB=off
    export GOTOOLCHAIN=local
    HELMR_BUN_ARCHIVE=${helmrPackages.standardToolchain.bunArchive} \
    HELMR_BUN_ARCHITECTURE=${lib.escapeShellArg helmrPackages.standardToolchain.architecture} \
      go test ./internal/deployment -run '^TestOfficialBunLoaderContract$'
  '';
  managed-runtime-contract =
    pkgs.runCommand "managed-runtime-contract-check"
      {
        nativeBuildInputs = [
          pkgs.go_1_26
          pkgs.proot
          pkgs.stdenv.cc
          pkgs.jq
          helmrPackages.squashfsTools
        ];
        src = ../.;
      }
      ''
        cp -R "$src" source
        chmod -R u+w source
        cd source
        export HOME="$TMPDIR/home"
        mkdir -p "$HOME" "$TMPDIR/verify"

        cp ${helmrPackages.managedRuntime}/runtime.squashfs "$TMPDIR/verify/runtime.squashfs"
        cp ${helmrPackages.managedRuntime}/runtime.descriptor.json "$TMPDIR/verify/runtime.descriptor.json"
        cat >internal/deployment/runtime_release_test.go <<'EOF'
        package deployment

        import (
          "context"
          "os"
          "testing"
        )

        func TestManagedRuntimeRelease(t *testing.T) {
          descriptorBytes, err := os.ReadFile(os.Getenv("HELMR_RUNTIME_DESCRIPTOR"))
          if err != nil {
            t.Fatal(err)
          }
          descriptor, err := ParseRuntimeDescriptor(descriptorBytes)
          if err != nil {
            t.Fatal(err)
          }
          image, err := os.Open(os.Getenv("HELMR_RUNTIME_IMAGE"))
          if err != nil {
            t.Fatal(err)
          }
          defer image.Close()
          snapshot, err := SnapshotRuntimeArtifact(
            context.Background(),
            os.Getenv("HELMR_RUNTIME_SNAPSHOT"),
            descriptor,
            image,
          )
          if err != nil {
            t.Fatal(err)
          }
          defer snapshot.Close()
          source, storedDescriptor, err := snapshot.verifier()
          if err != nil {
            t.Fatal(err)
          }
          stat, err := source.Stat()
          if err != nil {
            t.Fatal(err)
          }
          if stat.Size() != descriptor.SizeBytes {
            t.Fatalf("snapshot size = %d, want %d", stat.Size(), descriptor.SizeBytes)
          }
          digest, err := digestRuntimeArtifact(
            context.Background(),
            source,
            stat.Size(),
          )
          if err != nil {
            t.Fatal(err)
          }
          if digest != descriptor.Digest || storedDescriptor != descriptor {
            t.Fatal("runtime snapshot descriptor does not match its bytes")
          }
          index, err := verifyRuntimeArtifactReader(
            context.Background(),
            source,
            stat.Size(),
          )
          if err != nil {
            t.Fatal(err)
          }
          canonical, err := CanonicalRuntimeIndex(index)
          if err != nil {
            t.Fatal(err)
          }
          if _, err := verifiedRuntimeResult(canonical, descriptor); err != nil {
            t.Fatal(err)
          }
        }
        EOF
        gofmt -w internal/deployment/runtime_release_test.go
        mkdir -p "$TMPDIR/verify/snapshot"
        cp -R --reflink=auto ${helmrPackages.helmr.goModules} vendor
        export GOFLAGS=-mod=vendor
        export GOPROXY=off
        export GOSUMDB=off
        export GOTOOLCHAIN=local
        export HELMR_RUNTIME_DESCRIPTOR="$TMPDIR/verify/runtime.descriptor.json"
        export HELMR_RUNTIME_IMAGE="$TMPDIR/verify/runtime.squashfs"
        export HELMR_RUNTIME_SNAPSHOT="$TMPDIR/verify/snapshot"
        go test ./internal/deployment -run '^TestManagedRuntimeRelease$'

        root="$TMPDIR/root"
        runtime="$TMPDIR/runtime"
        program="$TMPDIR/program"
        mkdir -p \
          "$root/etc" \
          "$root/lib" \
          "$runtime" \
          "$program/pkg"
        unsquashfs -no-progress -d "$runtime" "$TMPDIR/verify/runtime.squashfs"
        case "$(jq -r '.architecture' "$TMPDIR/verify/runtime.descriptor.json")" in
          x86_64)
            managed_runtime_loader=ld-linux-x86-64.so.2
            managed_runtime_loader_source=${lib.getLib pkgs.glibc}/lib/ld-linux-x86-64.so.2
            ;;
          aarch64)
            managed_runtime_loader=ld-linux-aarch64.so.1
            managed_runtime_loader_source=${lib.getLib pkgs.glibc}/lib/ld-linux-aarch64.so.1
            ;;
          *)
            echo "runtime descriptor has unsupported architecture" >&2
            exit 1
            ;;
        esac

        manifest_line() {
          path="$1"
          relative="''${path#"$runtime/"}"
          if [ -d "$path" ]; then
            printf 'd\t0755\t%s\n' "$relative"
          elif [ -L "$path" ]; then
            printf 'l\t0777\t%s\t%s\n' "$relative" "$(readlink "$path")"
          else
            mode=0644
            case "$relative" in
              bin/node|lib/$managed_runtime_loader) mode=0755 ;;
            esac
            printf 'f\t%s\t%s\t%s\n' \
              "$mode" "$(sha256sum "$path" | cut -d' ' -f1)" "$relative"
          fi
        }
        while IFS= read -r path; do
          manifest_line "$path"
        done < <(find "$runtime" -mindepth 1 -print | LC_ALL=C sort) \
          >"$TMPDIR/verify/reconstructed.manifest"
        cmp \
          ${helmrPackages.managedRuntime}/runtime.manifest \
          "$TMPDIR/verify/reconstructed.manifest"
        cmp \
          "$managed_runtime_loader_source" \
          "$runtime/lib/$managed_runtime_loader"

        printf '%s\n' \
          '127.0.0.1 localhost' \
          '::1 localhost' \
          >"$root/etc/hosts"
        printf '%s\n' \
          'nameserver 127.0.0.1' \
          'options attempts:1 timeout:1' \
          >"$root/etc/resolv.conf"
        printf '%s\n' 'hosts: evil files dns' >"$root/etc/nsswitch.conf"
        chmod a-w "$runtime/lib/nsswitch.conf"
        cat >"$program/pkg/esm.ts" <<'EOF'
        enum Label {
          ESM = "raw-esm",
        }
        export const esmValue: Label = Label.ESM;
        EOF
        cat >"$program/pkg/index.ts" <<'EOF'
        interface Value {
          label: string;
        }
        const value: Value = { label: "raw-index" };
        export const indexValue = value.label;
        EOF
        cat >"$program/pkg/common.cts" <<'EOF'
        enum Label {
          CommonJS = "raw-commonjs",
        }
        module.exports = { commonValue: Label.CommonJS };
        EOF
        cat >"$program/worker.mjs" <<'EOF'
        import { parentPort } from "node:worker_threads";
        import { esmValue } from "./pkg/esm.ts";

        parentPort.postMessage(esmValue);
        EOF
        cat >"$program/probe.mjs" <<'EOF'
        import assert from "node:assert/strict";
        import { createHash } from "node:crypto";
        import dgram from "node:dgram";
        import dns from "node:dns/promises";
        import { createRequire } from "node:module";
        import tls from "node:tls";
        import { Worker } from "node:worker_threads";
        import { esmValue } from "./pkg/esm.ts";
        import { indexValue } from "./pkg/index.ts";

        assert.equal(process.execPath, "/opt/helmr/runtime/bin/node");
        assert.deepEqual(process.execArgv, [
          "--experimental-transform-types",
          "--import=file:///opt/helmr/runtime/helmr/preload.mjs",
        ]);
        assert.equal(process.env.NODE_OPTIONS, undefined);
        assert.equal(process.env.NODE_PATH, undefined);
        assert.equal(process.env.NODE_EXTRA_CA_CERTS, undefined);
        assert.equal(process.env.NODE_ICU_DATA, undefined);
        assert.equal(process.env.LD_PRELOAD, undefined);
        assert.equal(process.env.NODE_ENV, "production");
        assert.equal(process.env.PATH, "/workspace/bin:/usr/bin");
        assert.equal(process.env.SSL_CERT_FILE, undefined);
        assert.equal(process.env.SSL_CERT_DIR, undefined);
        assert.equal(process.env.OPENSSL_CONF, undefined);
        assert.equal(process.env.OPENSSL_MODULES, undefined);
        assert.equal(process.env.OPENSSL_ENGINES, undefined);
        assert.equal(process.env.GCONV_PATH, undefined);
        assert.equal(process.env.LOCPATH, undefined);
        assert.equal(
          Boolean(process.config.variables.node_use_openssl_ca),
          false,
        );
        assert.equal(esmValue, "raw-esm");
        assert.equal(indexValue, "raw-index");
        const require = createRequire(import.meta.url);
        assert.deepEqual(require("./pkg/common.cts"), {
          commonValue: "raw-commonjs",
        });
        assert.match(require.resolve("./pkg/common.cts"), /\/pkg\/common\.cts$/);
        const workerValue = await new Promise((resolve, reject) => {
          const worker = new Worker(new URL("./worker.mjs", import.meta.url));
          worker.once("message", resolve);
          worker.once("error", reject);
        });
        assert.equal(workerValue, "raw-esm");
        assert.equal(new Intl.DateTimeFormat("ja-JP").resolvedOptions().locale, "ja-JP");
        assert.equal(
          new TextDecoder("shift_jis").decode(
            Uint8Array.from([0x82, 0xb1, 0x82, 0xf1, 0x82, 0xc9, 0x82, 0xbf, 0x82, 0xcd]),
          ),
          "こんにちは",
        );
        assert.equal((await dns.lookup("localhost")).address.length > 0, true);
        const dnsServer = dgram.createSocket("udp4");
        dnsServer.on("message", (query, remote) => {
          let offset = 12;
          while (query[offset] !== 0) {
            offset += query[offset] + 1;
          }
          const questionEnd = offset + 5;
          const response = Buffer.alloc(questionEnd + 16);
          query.copy(response, 0, 0, questionEnd);
          response.writeUInt16BE(0x8180, 2);
          response.writeUInt16BE(1, 4);
          response.writeUInt16BE(1, 6);
          response.writeUInt16BE(0, 8);
          response.writeUInt16BE(0, 10);
          response.writeUInt16BE(0xc00c, questionEnd);
          response.writeUInt16BE(1, questionEnd + 2);
          response.writeUInt16BE(1, questionEnd + 4);
          response.writeUInt32BE(60, questionEnd + 6);
          response.writeUInt16BE(4, questionEnd + 10);
          response.set([192, 0, 2, 1], questionEnd + 12);
          dnsServer.send(response, remote.port, remote.address);
        });
        await new Promise((resolve) => dnsServer.bind(53535, "127.0.0.1", resolve));
        assert.deepEqual(
          await dns.lookup("helmr-runtime.test", { family: 4 }),
          { address: "192.0.2.1", family: 4 },
        );
        await new Promise((resolve) => dnsServer.close(resolve));
        assert.equal(tls.rootCertificates.length > 0, true);
        tls.createSecureContext();
        assert.equal(
          createHash("sha256").update("helmr").digest("hex"),
          "9d06c282b54c131bd2981a2e45b4345c1f3d52d83fddac0fba7d616cc0d61cd3",
        );
        console.log("runtime probe passed");
        EOF

        cat >"$TMPDIR/evil.c" <<'EOF'
        #include <fcntl.h>
        #include <unistd.h>

        __attribute__((constructor)) static void loaded(void) {
          int fd = open("/evil-loaded", O_WRONLY | O_CREAT, 0600);
          if (fd >= 0) {
            close(fd);
          }
        }
        EOF
        "$CC" -shared -fPIC "$TMPDIR/evil.c" \
          -o "$runtime/lib/libnss_evil.so.2"

        base_proot=(
          -r "$root"
          -p 53:53535
          -b /dev
          -b /proc
          -b "$runtime:/opt/helmr/runtime"
          -b "$program:/opt/helmr/program"
        )
        common_proot=(
          "''${base_proot[@]}"
          -b "$runtime/lib/nsswitch.conf:/etc/nsswitch.conf"
        )
        proot "''${common_proot[@]}" \
          "/opt/helmr/runtime/lib/$managed_runtime_loader" \
          --library-path /opt/helmr/runtime/lib \
          /opt/helmr/runtime/bin/node \
          --version
        clean_runtime_env=(
          -u NODE_OPTIONS
          -u NODE_PATH
          -u NODE_EXTRA_CA_CERTS
          -u NODE_ICU_DATA
          -u SSL_CERT_FILE
          -u SSL_CERT_DIR
          -u OPENSSL_CONF
          -u OPENSSL_MODULES
          -u OPENSSL_ENGINES
          -u GCONV_PATH
          -u LOCPATH
          -u LD_PRELOAD
          -u LD_LIBRARY_PATH
          NODE_ENV=production
          PATH=/workspace/bin:/usr/bin
        )
        env "''${clean_runtime_env[@]}" \
          ${pkgs.proot}/bin/proot "''${base_proot[@]}" \
          /opt/helmr/runtime/bin/node \
          --experimental-transform-types \
          --import=file:///opt/helmr/runtime/helmr/preload.mjs \
          /opt/helmr/program/probe.mjs
        test -e "$root/evil-loaded"
        rm "$root/evil-loaded"
        env "''${clean_runtime_env[@]}" \
          ${pkgs.proot}/bin/proot "''${common_proot[@]}" \
          /opt/helmr/runtime/bin/node \
          --experimental-transform-types \
          --import=file:///opt/helmr/runtime/helmr/preload.mjs \
          /opt/helmr/program/probe.mjs
        test ! -e "$root/evil-loaded"

        cli_root="$TMPDIR/cli-root"
        cli_program="$TMPDIR/cli-program"
        install -d \
          "$cli_root/bin" \
          "$cli_root/usr/bin" \
          "$cli_program/node_modules/.bin" \
          "$cli_program/node_modules/tools"
        ln -s ${pkgs.bash}/bin/bash "$cli_root/bin/sh"
        ln -s ${pkgs.coreutils}/bin/env "$cli_root/usr/bin/env"
        cat >"$cli_program/node_modules/tools/node-tool" <<'EOF'
        #!/usr/bin/env node
        process.stdout.write("node-tool\n");
        EOF
        cat >"$cli_program/node_modules/tools/shell-tool" <<'EOF'
        #!/bin/sh
        printf '%s\n' shell-tool
        EOF
        cat >"$cli_program/node_modules/tools/python-tool" <<'EOF'
        #!/usr/bin/env python3
        print("python-tool")
        EOF
        cat >"$cli_program/node_modules/tools/missing-tool" <<'EOF'
        #!/workspace/missing-interpreter
        EOF
        cp ${pkgs.hello}/bin/hello "$cli_program/node_modules/tools/native-tool"
        chmod 0755 "$cli_program/node_modules/tools/"*
        for name in node-tool shell-tool python-tool native-tool missing-tool; do
          ln -s "../tools/$name" "$cli_program/node_modules/.bin/$name"
        done
        cli_proot=(
          -r "$cli_root"
          -b /dev
          -b /proc
          -b /nix/store
          -b "$cli_program:/opt/helmr/program"
        )
        cli_path="${pkgs.nodejs_24}/bin:${pkgs.python3}/bin:${pkgs.coreutils}/bin"
        test "$(
          env PATH="$cli_path" ${pkgs.proot}/bin/proot "''${cli_proot[@]}" \
            /opt/helmr/program/node_modules/.bin/node-tool
        )" = node-tool
        test "$(
          env PATH="$cli_path" ${pkgs.proot}/bin/proot "''${cli_proot[@]}" \
            /opt/helmr/program/node_modules/.bin/shell-tool
        )" = shell-tool
        test "$(
          env PATH="$cli_path" ${pkgs.proot}/bin/proot "''${cli_proot[@]}" \
            /opt/helmr/program/node_modules/.bin/python-tool
        )" = python-tool
        test "$(
          env PATH="$cli_path" ${pkgs.proot}/bin/proot "''${cli_proot[@]}" \
            /opt/helmr/program/node_modules/.bin/native-tool
        )" = "Hello, world!"
        if env PATH="$cli_path" ${pkgs.proot}/bin/proot "''${cli_proot[@]}" \
          /opt/helmr/program/node_modules/.bin/missing-tool; then
          echo "package CLI with a missing Workspace interpreter succeeded" >&2
          exit 1
        fi

        jq -e '
          keys == [
            "architecture",
            "digest",
            "formatVersion",
            "mediaType",
            "runtimeApiVersion",
            "sizeBytes"
          ]
        ' ${helmrPackages.managedRuntime}/runtime.descriptor.json >/dev/null
        test "$(tail -c1 ${helmrPackages.managedRuntime}/runtime.descriptor.json | od -An -tuC | tr -d ' ')" != 10
        test "$(tail -c1 "$runtime/helmr/runtime.json" | od -An -tuC | tr -d ' ')" != 10
        test "$(find "$runtime" -mindepth 1 -maxdepth 1 -printf '%f\n' | LC_ALL=C sort | tr '\n' ' ')" = "bin helmr lib "
        test "$(find "$runtime/bin" -mindepth 1 -maxdepth 1 -printf '%f\n' | LC_ALL=C sort | tr '\n' ' ')" = "node "
        test "$(find "$runtime/helmr" -mindepth 1 -maxdepth 1 -printf '%f\n' | LC_ALL=C sort | tr '\n' ' ')" = "preload.mjs runtime.json "
        test ! -e "$runtime/lib/gconv"
        test ! -e "$runtime/lib/locale"
        test ! -e "$runtime/lib/openssl"
        test ! -e "$runtime/lib/ossl-modules"
        test ! -e "$runtime/lib/libnss_files.so.2"
        test ! -e "$runtime/lib/libnss_dns.so.2"
        test ! -e "$runtime/lib/libresolv.so.2"
        test "$(grep -Ec '(^|[[:space:]])(compat|db|hesiod|systemd)([[:space:]]|$)' "$runtime/lib/nsswitch.conf")" = 0

        touch "$out"
      '';
}
