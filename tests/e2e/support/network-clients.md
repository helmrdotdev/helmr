# Native HTTPS client qualification

`network-clients.mjs` is the same guest program for a disposable Linux namespace
and a Helmr Computer. It uses the existing pinned Anthropic TypeScript SDK against
synthetic TLS responses, native Git over HTTPS, native npm with a scoped synthetic
registry token and two cross-origin redirects, a forbidden npm redirect, a numeric-IP
TLS connection with forbidden SNI, and private HTTPS that fails without and succeeds
with explicit fixture CA trust.

The SDK must receive and acknowledge the first text chunk before the endpoint
releases the second. A fresh per-request gate preserves this assertion across
repeated runs. Each npm attempt uses a fresh cache and install prefix: identical
cached tarball integrity must never count as a denied-network probe. The server
also checks that registry credentials do not reach the auth/CDN origins.

This proves native client transport behavior against a controlled endpoint. It
does not prove live model-provider responses, provider authentication, inference,
external registry availability or every real provider's redirect/CDN requirements.
Namespace proof does not qualify a VM boot, adoption, checkpoint or restore.

## Client and artifact prerequisites

Use Node 24.22.1 from `internal/version/runtime-dependencies.json`, Git 2.51.2 from
the repository's pinned Nix input, npm 11.19.1, and `@anthropic-ai/sdk` 0.98.0 from
`examples/issue-fixer`'s lock. Do not use an arbitrary installed SDK or publish a
package. The existing SDK lock records this package integrity:

```text
sha512-N7aXtCvC5g6T1Y4V29lJjceu/zTkVkIZF0jdBvagr0TRFHuKeImffalGWEfqZKrvjH+IQbzJWw6TmSmUzrlMgg==
```

Prepare its locked dependency closure outside the checkout using the native
package manager with scripts disabled and isolated fixture-only npm config/cache.
Reuse that exact closure offline in both environments. The runner requires
`HELMR_NETWORK_CLIENT_SDK_ROOT` to identify the installed package directory; its
`index.mjs`, package metadata and dependencies must be present.

The endpoint records actual `node --version`, `npm --version` and `git --version`
results. The runner requires identical versions and SDK 0.98.0, and emits those
actual versions, endpoint test-executable SHA256, its own SHA256, SDK
package-metadata SHA256 and fixture-CA SHA256.
The actual npm bundled with the exercised Node 24.22.1 archive was 11.19.1; this is
an observed tool version, not inferred from a package declaration. Keep the
artifact/core revision, harness revision, test executable hash and receipt with
qualification evidence. A matching version is not a matching release artifact.

The runner creates and removes its own temporary directory and supplies explicit
child HOME, Git config, npm user/global config and cache paths. It does not change
ambient shell HOME or CODEX_HOME and does not inspect real credentials. The only
credential is the public synthetic fixture token. CA trust is process/command
specific; there is no system CA installation or certificate-verification bypass.

## Disposable local Linux namespace

On an explicitly selected disposable privileged Linux environment with pinned
Node/npm, Git, nftables and iproute2 on PATH:

```sh
nix develop --command env CGO_ENABLED=0 go test -c \
  -o /private/attempt/network-clients.test ./internal/firecracker
HELMR_TEST_NAMESPACE=1 \
HELMR_NETWORK_CLIENT_SCRIPT="$PWD/tests/e2e/support/network-clients.mjs" \
HELMR_NETWORK_CLIENT_SDK_ROOT=/private/fixtures/sdk/node_modules/@anthropic-ai/sdk \
/private/attempt/network-clients.test \
  -test.run '^TestNetworkRealClientsNamespace$' -test.v -test.timeout 90s
```

For cross compilation set the target GOOS/GOARCH and execute the resulting binary
in the Linux container. The script, SDK closure and native client tools must be
available there. Use a network-none container; copying pinned tools into its
private filesystem does not require external client traffic.

The test creates separate guest, gateway and endpoint namespaces. Endpoint
addresses belong only to the endpoint namespace; actual host/interface prefixes
remain denied. Its public-shaped address and explicitly approved private address
are routed through a test-owned veth. Real nftables/TPROXY and selective DNS direct
guest-root traffic through the current gateway. No proxy environment variables or
host/interface/metadata/peer deny exceptions are introduced. The same endpoint serves two independent client runs. Every namespace,
route-bearing link and temporary resolver configuration has test-owned cleanup.

## Prepared route for a real Computer

This is source preparation, not authorization to change a Worker or deploy a
fixture. Use the existing dedicated-host operation boundary before changing its
fixture routing/DNS or executing a workload.

1. Serve the endpoint in an **independent routed fixture namespace or service**
   with a dedicated RFC1918 address, for example `10.60.1.2`. Do not bind a Worker
   host/interface address or loosen management, metadata or peer denies to make
   it reachable. Keep the fixture address outside those ranges. Customer routing
   and selected-resolver A records must already be configured through the approved
   host operation.
2. In that endpoint namespace, put the pinned client tools on PATH and run the
   separately built harness binary. The output directory must be new:

   ```sh
   HELMR_NETWORK_CLIENT_SERVER_DIR=/private/attempt/network-client-endpoint \
   HELMR_NETWORK_CLIENT_BIND_IPV4=10.60.1.2 \
   /private/attempt/network-clients.test \
     -test.run '^TestNetworkClientEndpoint$' -test.v -test.timeout 30m
   ```

   The endpoint produces `config.json` and the public `ca.pem`. Keep its private
   keys in the serving process. Stop it with SIGTERM after owned work completes.
3. Configure the installation business ceiling to include the fixture address
   only through its approved drained rollout. In a dedicated test Template,
   grant these exact names, each with `ipRanges: ["10.60.1.2/32"]`:

   ```text
   model.network.test
   git.network.test
   registry.network.test
   auth.network.test
   cdn.network.test
   private.network.test
   ```

   The selected resolver must map those names and `denied.network.test` to the
   independent fixture address. The denied name must resolve upstream so that
   policy is the only blocker; it stays unlisted in the allowlist. The runner also
   connects to the registry's numeric IPv4 address with that denied SNI, bypassing
   DNS for the denied name. Both probes require zero denied-origin contacts.
   No Secret binding grants connectivity.
4. Prepare the test Computer image with the same offline native client/SDK
   artifacts. Stage the unchanged `network-clients.mjs`, endpoint `config.json`
   and public `ca.pem` in one owned guest directory. The configuration's relative
   `caFile` remains valid after copying. Supply the fixture CA only to these
   clients, not globally. Missing tools, config, CA or SDK are failed prerequisites.
5. Execute the unchanged runner through the existing Computer command API. For
   example, with the exact toolchain on the prepared image's PATH:

   ```ts
   const command = await computer.exec({
     command: ["node", "/workspace/network-clients/network-clients.mjs"],
     env: {
       PATH: fixtureToolPath,
       HELMR_NETWORK_CLIENT_CONFIG: "/workspace/network-clients/config.json",
       HELMR_NETWORK_CLIENT_SDK_ROOT: "/opt/network-client-sdk/node_modules/@anthropic-ai/sdk",
       NODE_EXTRA_CA_CERTS: "/workspace/network-clients/ca.pem",
     },
     timeout: "2m",
     idempotencyKey: uniqueClientProbeKey,
   })
   const outcome = await command.wait()
   if (outcome.kind !== "exited" || outcome.exitCode !== 0) throw new Error("Native client matrix failed")
   for await (const record of command.logs.stream()) {
     if (record.kind === "gap") throw new Error("Native client evidence gap")
     // Persist stdout/stderr with the exact Computer/Deployment and core release.
   }
   ```

   Use existing read/exec authority; the helper does not create deployments,
   Computers, cloud services or credentials. Run again after the specifically
   selected adoption/restore operation with a new command idempotency key, and
   compare the actual client versions and artifact/runner hashes. Clean up owned
   fixture resources through normal APIs and stop the endpoint explicitly.
