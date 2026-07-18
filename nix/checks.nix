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

  helmrShellCheck =
    pkgs.runCommand "managed-runtime-shell-check"
      {
        nativeBuildInputs = [
          pkgs.bash
          pkgs.stdenv.cc
          pkgs.gnugrep
        ];
        src = ../.;
      }
      ''
        bash "$src/runtime/managed/helmr-sh.test.sh"
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
  managed-runtime-shell = helmrShellCheck;
  squashfs-tools = helmrPackages.squashfsTools;
}
// lib.optionalAttrs pkgs.stdenv.isLinux {
  firecracker-host-module = firecrackerHostModuleCheck;
  managed-runtime = helmrPackages.managedRuntime;
  managed-runtime-contract =
    pkgs.runCommand "managed-runtime-contract-check"
      {
        nativeBuildInputs = [
          pkgs.go_1_26
          pkgs.proot
          pkgs.stdenv.cc
          pkgs.patchelf
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
          "$program/helmr/files/modules" \
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
              bin/node|bin/helmr-sh|lib/$managed_runtime_loader) mode=0755 ;;
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

        cp /etc/hosts "$root/etc/hosts"
        cp /etc/resolv.conf "$root/etc/resolv.conf"
        printf '%s\n' 'hosts: evil files dns' >"$root/etc/nsswitch.conf"
        chmod a-w "$runtime/lib/nsswitch.conf"
        cat >"$program/pkg/esm.ts" <<'EOF'
        throw new Error("raw ESM source executed");
        EOF
        cat >"$program/pkg/index.ts" <<'EOF'
        throw new Error("raw index source executed");
        EOF
        cat >"$program/pkg/common.cts" <<'EOF'
        throw new Error("raw CommonJS source executed");
        EOF
        cat >"$program/helmr/files/modules/esm.mjs" <<'EOF'
        export const esmValue = "mapped-esm";
        EOF
        cat >"$program/helmr/files/modules/index.mjs" <<'EOF'
        export const indexValue = "mapped-index";
        EOF
        cat >"$program/helmr/files/modules/common.cjs" <<'EOF'
        module.exports = { commonValue: "mapped-commonjs" };
        EOF
        cat >"$program/worker.mjs" <<'EOF'
        import { parentPort } from "node:worker_threads";
        import { esmValue } from "./pkg/esm";

        parentPort.postMessage(esmValue);
        EOF
        cat >"$program/helmr/modules.json" <<'EOF'
        {"formatVersion":0,"modules":[{"codePath":"helmr/files/modules/common.cjs","format":"commonjs","path":"pkg/common.cts"},{"codePath":"helmr/files/modules/esm.mjs","format":"module","path":"pkg/esm.ts"},{"codePath":"helmr/files/modules/index.mjs","format":"module","path":"pkg/index.ts"}],"transformer":"helmr.typescript.v0"}
        EOF
        cat >"$program/probe.mjs" <<'EOF'
        import assert from "node:assert/strict";
        import dns from "node:dns/promises";
        import { createRequire } from "node:module";
        import tls from "node:tls";
        import { Worker } from "node:worker_threads";
        import { esmValue } from "./pkg/esm";
        import { indexValue } from "./pkg";

        assert.equal(process.execPath, "/opt/helmr/runtime/bin/node");
        assert.deepEqual(process.execArgv, [
          "--import=file:///opt/helmr/runtime/helmr/preload.mjs",
        ]);
        assert.equal(process.env.NODE_OPTIONS, undefined);
        assert.equal(process.env.LD_PRELOAD, undefined);
        assert.equal(process.env.NODE_ENV, "production");
        assert.equal(process.env.GCONV_PATH, "/opt/helmr/runtime/lib/gconv");
        assert.equal(process.env.LOCPATH, "/opt/helmr/runtime/lib/locale");
        assert.equal(
          process.env.OPENSSL_CONF,
          "/opt/helmr/runtime/lib/openssl/openssl.cnf",
        );
        assert.equal(
          process.env.OPENSSL_MODULES,
          "/opt/helmr/runtime/lib/ossl-modules",
        );
        assert.equal(
          Boolean(process.config.variables.node_use_openssl_ca),
          false,
        );
        assert.equal(esmValue, "mapped-esm");
        assert.equal(indexValue, "mapped-index");
        const require = createRequire(import.meta.url);
        assert.deepEqual(require("./pkg/common"), {
          commonValue: "mapped-commonjs",
        });
        assert.match(require.resolve("./pkg/common"), /\/pkg\/common\.cts$/);
        const workerValue = await new Promise((resolve, reject) => {
          const worker = new Worker(new URL("./worker.mjs", import.meta.url));
          worker.once("message", resolve);
          worker.once("error", reject);
        });
        assert.equal(workerValue, "mapped-esm");
        assert.equal(new Intl.DateTimeFormat("en-US").resolvedOptions().locale, "en-US");
        assert.equal((await dns.lookup("localhost")).address.length > 0, true);
        assert.equal(tls.rootCertificates.length > 0, true);
        tls.createSecureContext();
        console.log("runtime probe passed");
        EOF
        cat >"$root/shim" <<'EOF'
        #!/opt/helmr/runtime/bin/helmr-sh
        exec /opt/helmr/runtime/bin/node --import=file:///opt/helmr/runtime/helmr/preload.mjs '/opt/helmr/program/probe.mjs' "$@"
        EOF
        chmod 0755 "$root/shim"

        cat >"$TMPDIR/iconv.c" <<'EOF'
        #include <errno.h>
        #include <iconv.h>
        #include <stddef.h>

        int main(void) {
          iconv_t converter = iconv_open("ISO-8859-1", "UTF-8");
          if (converter == (iconv_t)-1) {
            return errno == 0 ? 2 : 1;
          }
          return iconv_close(converter) == 0 ? 0 : 3;
        }
        EOF
        "$CC" "$TMPDIR/iconv.c" -o "$root/test-iconv"
        patchelf \
          --set-interpreter "/opt/helmr/runtime/lib/${
            if pkgs.stdenv.hostPlatform.isx86_64 then "ld-linux-x86-64.so.2" else "ld-linux-aarch64.so.1"
          }" \
          --set-rpath /opt/helmr/runtime/lib \
          "$root/test-iconv"

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
        env NODE_ENV=production \
          proot "''${base_proot[@]}" /opt/helmr/runtime/bin/helmr-sh /shim
        test -e "$root/evil-loaded"
        rm "$root/evil-loaded"
        env \
          NODE_OPTIONS=hostile \
          NODE_PATH=hostile \
          NODE_EXTRA_CA_CERTS=hostile \
          NODE_ICU_DATA=hostile \
          SSL_CERT_FILE=hostile \
          SSL_CERT_DIR=hostile \
          OPENSSL_CONF=hostile \
          OPENSSL_MODULES=hostile \
          OPENSSL_ENGINES=hostile \
          GCONV_PATH=hostile \
          LOCPATH=hostile \
          LD_PRELOAD= \
          NODE_ENV=production \
          proot "''${common_proot[@]}" /opt/helmr/runtime/bin/helmr-sh /shim
        env \
          GCONV_PATH=/opt/helmr/runtime/lib/gconv \
          LOCPATH=/opt/helmr/runtime/lib/locale \
          proot "''${common_proot[@]}" /test-iconv
        test ! -e "$root/evil-loaded"

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
        test "$(find "$runtime/bin" -mindepth 1 -maxdepth 1 -printf '%f\n' | LC_ALL=C sort | tr '\n' ' ')" = "helmr-sh node "
        test "$(find "$runtime/helmr" -mindepth 1 -maxdepth 1 -printf '%f\n' | LC_ALL=C sort | tr '\n' ' ')" = "preload.mjs runtime.json "
        test "$(grep -Ec '(^|[[:space:]])(compat|db|hesiod|systemd)([[:space:]]|$)' "$runtime/lib/nsswitch.conf")" = 0

        touch "$out"
      '';
}
