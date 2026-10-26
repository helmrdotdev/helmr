# Native client traffic across Computer restore

This case runs the unchanged [native client runner](../../support/network-clients.md)
on one finite-network Computer before and after explicit Hibernate/Wake. It checks
the exact durable checkpoint, source fencing, replacement Instance, same Host,
retained image/seed identities and reconciled controls. It records both native
client receipts and requires stable versions and artifact hashes. Normal case
cleanup deletes its Computer; endpoint, DNS and host lifetime remain operator-owned.
This does not exercise a live model provider or physical cross-host restore.

## Offline image inputs

Binary payloads belong only in a new outside-checkout prepared project. The image
copies one archive and installs it without network access. It contains the official
Product-pinned Linux x64 Node archive (including npm), the pinned Nix Git closure,
and the already locked SDK dependency closure. No credential or system CA belongs
in the archive. The base Debian image digest matches the repository runtime-library
pin. The image build checks Node 24.22.1, npm 11.19.1 and Git 2.51.2.

Acquire the Node archive using the URL and x86_64-linux SHA256 in
`internal/version/runtime-dependencies.json`. Obtain Git from this checkout's
`nixpkgs` input, for example on the selected Linux x86_64 build environment:

```sh
git_store=$(nix build --impure --no-link --print-out-paths --expr \
  'let f = builtins.getFlake ("git+file://" + builtins.toString ./.); in (import f.inputs.nixpkgs { system = "x86_64-linux"; }).git')
```

Use the isolated SDK project described in the runner prerequisites. Its installed
SDK must be 0.98.0, and its npm lock must retain the integrity recorded in
`examples/issue-fixer/bun.lock`. Reuse the same exact closure; do not install the
entire issue-fixer example or resolve a fresh SDK version inside the Computer.
`package-tools.py` verifies the official Node archive hash, bundled npm version,
Git architecture/version path, SDK version and recorded integrity. It packages the
existing closure; it does not reacquire packages or independently authenticate an
arbitrarily modified installed SDK directory. Retain acquisition integrity evidence.

```sh
nix develop -c python3 tests/e2e/cases/network-clients/package-tools.py \
  --node-archive /private/fixtures/node-v24.22.1-linux-x64.tar.xz \
  --git-store-path "$git_store" \
  --sdk-directory /private/fixtures/network-client-sdk \
  --output /private/fixtures/native-clients.tar.gz
nix develop -c python3 tests/e2e/prepare_project.py /private/client-project \
  --fixtures cases/network-clients --sdk-packages /private/packed-sdk
cp /private/fixtures/native-clients.tar.gz \
  /private/client-project/cases/network-clients/native-clients.tar.gz
```

Keep the packager's JSON receipt and archive SHA256 with the prepared project's
build evidence. Normal `helmr build`/`helmr deploy` consume that outside-checkout
project. Build source is limited to 512 MiB; keep the compressed archive within
that existing limit. The guest tool paths are entirely image-owned under
`/opt/network-client-tools`; no host mount or host PATH is supplied at runtime.
The same archive may supply the independently routed endpoint's native tools.

## Endpoint, routing and selected DNS

Use the dedicated-host authorization and profile update boundary first. These
commands are explicit setup operations, not work performed by the case. Inventory
the actual addresses, routes, pools and deny ranges first. The sample addresses
must be unused and outside all management, metadata, peer and link-pool denies.
Never bind fixture HTTPS to a Worker host-interface address.

On the dedicated host, with the trusted pinned `ip` tool:

```sh
sudo ip netns add helmr-client-endpoint
sudo ip link add hncli-host type veth peer name hncli-end netns helmr-client-endpoint
sudo ip addr add 100.111.0.1/30 dev hncli-host
sudo ip link set hncli-host up
sudo ip -n helmr-client-endpoint link set lo up
sudo ip -n helmr-client-endpoint addr add 100.111.0.2/30 dev hncli-end
sudo ip -n helmr-client-endpoint link set hncli-end up
sudo ip -n helmr-client-endpoint addr add 10.60.1.2/32 dev lo
sudo ip -n helmr-client-endpoint route add default via 100.111.0.1
sudo ip route add 10.60.1.2/32 via 100.111.0.2 dev hncli-host
```

The endpoint owns `10.60.1.2`; the Worker host owns only its transit address.
The gateway's upstream socket uses the host's route to the independent endpoint.
Do not add a deny exception or a forwarding/NAT bypass for HTTPS.

The selected resolver must be routed outside the Worker host. The
[ordinary DNS rules](../../../../internal/firecracker/network_policy_contract.go)
allow selected DNS through the allocation's forward chain, but the
[root policy](../../../../internal/firecracker/network_policy_linux.go) drops
allocation traffic addressed to the host itself. A resolver bound only to the
host's transit address cannot serve an ordinary Computer through that boundary.

Use the host's `100.111.0.1:53` resolver only as the fixture's upstream. It holds
the seven fixture records and forwards other names to the pre-fixture selected
resolver. `ORIGINAL_RESOLVER_IPV4` must resolve public names from the host and
must differ from both fixture resolvers, avoiding a forwarding loop. For example,
with a dnsmasq binary from the same pinned Nix input:

```sh
sudo systemd-run --unit=helmr-client-dns --collect --property=Type=exec \
  "$DNSMASQ_BIN" --keep-in-foreground --no-hosts --no-resolv --bind-interfaces \
  --listen-address=100.111.0.1 --server="$ORIGINAL_RESOLVER_IPV4" \
  --local=/network.test/ \
  --host-record=model.network.test,10.60.1.2 \
  --host-record=git.network.test,10.60.1.2 \
  --host-record=registry.network.test,10.60.1.2 \
  --host-record=auth.network.test,10.60.1.2 \
  --host-record=cdn.network.test,10.60.1.2 \
  --host-record=private.network.test,10.60.1.2 \
  --host-record=denied.network.test,10.60.1.2
```

Expose the selected resolver at the endpoint-owned `10.60.1.2:53`, forwarding to
that host upstream over the fixture veth. This second service joins the endpoint
network namespace; it needs no private tools mount because its pinned dnsmasq
binary uses the host filesystem:

```sh
sudo systemd-run --unit=helmr-client-endpoint-dns --collect --property=Type=exec \
  --property=NetworkNamespacePath=/run/netns/helmr-client-endpoint \
  "$DNSMASQ_BIN" --keep-in-foreground --no-hosts --no-resolv --bind-interfaces \
  --listen-address=10.60.1.2 --server=100.111.0.1
```

Before installing the profile, prove UDP and TCP DNS through `10.60.1.2` resolves
public names and all seven fixture names, including `denied.network.test`.
Checking only the host's upstream resolver is insufficient. The existing routed
forwarding and masquerade rules carry ordinary guest queries and established
replies; the endpoint resolver reaches its upstream over `hncli-host`, outside
the allocation interfaces. Preserve the root input/output drops, mandatory denies
and existing NAT rules without adding exceptions.

Configure these inputs before the approved fresh native profile installation:
both `CONTROL_PLANE_NETWORK_PRIVATE_BUSINESS_CIDRS` and
`WORKER_NETWORK_PRIVATE_BUSINESS_CIDRS` must include `10.60.1.2/32`, and
`WORKER_NETWORK_RESOLVER_IPV4` must select `10.60.1.2`. Preserve all existing
blocked ranges. Changing a retained profile's resolver requires its supported
retirement and fresh installation/enrollment, followed by normal setup. Do not
hand-edit saved profile/environment files; `apply-services` is not a Worker
configuration update API. Both DNS units remain independent of the Worker for
the case's drain/restart operation.

Build the endpoint executable from the exact selected source:

```sh
nix develop -c env CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go test -c -o /private/fixtures/network-clients.test ./internal/firecracker
```

Transfer that binary and the tools archive through the authorized host transport
and verify their hashes. Extract into a new owned private directory, never into
the retained host's root or its `/nix/store`. Require the retained host's existing
`/nix` directory. Record whether the `/opt` bind target already exists, and create
it only if absent. Keep this ownership boolean for teardown in the same operator
shell; stop setup if any command fails:

```sh
sudo test -d /nix
endpoint_bind_target_created=false
if sudo test -e /opt/network-client-tools || sudo test -L /opt/network-client-tools; then
  sudo test -d /opt/network-client-tools
  printf '%s\n' '/opt/network-client-tools: pre-existing; preserve at teardown'
else
  sudo mkdir /opt/network-client-tools
  endpoint_bind_target_created=true
  printf '%s\n' '/opt/network-client-tools: created for this attempt'
fi
ENDPOINT_TOOLS_ROOT=/private/attempt/client-endpoint-tools
sudo test ! -e "$ENDPOINT_TOOLS_ROOT"
sudo mkdir -m 0700 "$ENDPOINT_TOOLS_ROOT"
sudo tar -xzf /private/fixtures/native-clients.tar.gz -C "$ENDPOINT_TOOLS_ROOT"
```

The transient service exposes only that extraction's store and tools at the
archive's absolute paths inside a private mount namespace. It joins the existing
endpoint network namespace through systemd, so its entrypoint does not depend on
a host Nix `ip` executable hidden by the private store. The host's store and tools
remain unchanged. Use a new output directory whose parent exists. Complete image
build and deployment first, then start the endpoint immediately before the case.
Its 60-minute bound leaves margin beyond the driver's 35 minutes of bounded
commands and capture/restore waits plus startup:

```sh
sudo systemd-run --unit=helmr-client-endpoint --collect --property=Type=exec \
  --property=NetworkNamespacePath=/run/netns/helmr-client-endpoint \
  --property=TemporaryFileSystem=/nix \
  --property="BindReadOnlyPaths=$ENDPOINT_TOOLS_ROOT/nix/store:/nix/store $ENDPOINT_TOOLS_ROOT/opt/network-client-tools:/opt/network-client-tools" \
  --setenv=PATH=/opt/network-client-tools/node/bin:/opt/network-client-tools/git/bin:/usr/bin:/bin \
  --setenv=HELMR_NETWORK_CLIENT_SERVER_DIR=/private/attempt/client-endpoint \
  --setenv=HELMR_NETWORK_CLIENT_BIND_IPV4=10.60.1.2 \
  /private/fixtures/network-clients.test \
  -test.run '^TestNetworkClientEndpoint$' -test.v -test.timeout 60m
```

Wait for its ready log and `config.json`/`ca.pem`. Verify all seven names resolve
upstream to `10.60.1.2`, including the denied name. The denied name stays absent
from the Template policy. Verify direct endpoint TLS with its CA and each expected
SNI before trusting a later denial. A TLS-only prerequisite need not send an HTTP
request to the denied origin, which would invalidate the zero-contact assertion.
Both transient services remain independent of the Worker for restore testing.

## Run and clean up

Deploy the prepared project through the normal authenticated route, retain its
bundle digest and exact core/image identities, and run this driver on the dedicated
host with the existing environment-scoped API key:

```sh
HELMR_API_URL=http://127.0.0.1:58080 \
HELMR_RUNTIME_HOST_TOOL=/installed/product/dev/runtime/host.py \
HELMR_NETWORK_CLIENT_ENDPOINT_DIR=/private/attempt/client-endpoint \
HELMR_EXPECTED_BUNDLE_DIGEST=sha256:ACTUAL_BUNDLE_DIGEST \
HELMR_EVIDENCE_DIR=/private/attempt/client-result \
bun run /private/client-project/cases/network-clients/run.ts
```

Use a new evidence directory. Required authority is normal Computer creation,
exec, Hibernate/Wake, deletion and the existing read-only host observer. The driver
copies only runner/config/public CA through native exec stdin (below its 1 MiB
limit). No server private key, Secret binding, system CA installation, guest-host
filesystem mount or new API is needed. It uses one resident VM slot, one new
Computer and fresh command idempotency keys before and after restore.

After the case, check its result and owned Computer's physical release with the
normal `computer-state` observer. On interruption, retain `result.json`, finish its
known Computer cleanup through normal APIs and keep failed evidence. After all
selected case work has finished, stop the profile before removing its resolver.
Then stop the owned transient services, preserve their journals and remove the
owned route/link and namespace:

```sh
sudo python3 "$HELMR_RUNTIME_HOST_TOOL" stop
sudo systemctl stop helmr-client-endpoint.service \
  helmr-client-endpoint-dns.service helmr-client-dns.service
sudo ip route del 10.60.1.2/32 via 100.111.0.2 dev hncli-host
sudo ip link del hncli-host
sudo ip netns del helmr-client-endpoint
sudo rm -rf -- "$ENDPOINT_TOOLS_ROOT"
if [ "$endpoint_bind_target_created" = true ]; then
  sudo rmdir -- /opt/network-client-tools
fi
```

Remove only the exact new extraction directory created for this attempt, after
the endpoint service has stopped and its private mounts have disappeared. Remove
the host bind target only when this attempt created it, using `rmdir` so a nonempty
directory is preserved. Keep every pre-existing target and the host's `/nix` and
`/nix/store`; never recursively remove the host bind target or store paths.

Stop the Worker/profile before removing a resolver it still selects. If the
installation will continue running, first restore its original resolver and
private ceiling through the same approved profile operation. Verify namespace,
route, veth and service absence, then follow normal host-stop ownership. Removing
fixture files or receiving a Computer deletion receipt alone does not establish
physical release or stopped compute.
