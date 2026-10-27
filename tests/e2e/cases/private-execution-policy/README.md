# Private guest policy qualification

This synthetic fixture verifies ordinary private-business access, finite private
HTTPS access, and a blocked runtime. The finite Template prepares with `blockAll`;
the blocked Template prepares with the finite policy. Both preparation results are
written into the prepared filesystem and verified at runtime, including after
explicit Hibernate/Wake. The same ordinary-default Agent runs on each explicit
Computer, so the actual Computer policy must govern reused Sessions.

Only select this fixture after the deployment owner has authorized the configured
private endpoints and preparation traffic. No model credentials are used. It does
not provision infrastructure, change installation policy, alter capacity, or stop
services. Local checks do not qualify KVM or an AWS deployment.

## Configure the prerequisites

Edit `config.ts` **before** running `prepare_project.py`. Its example names and
addresses are synthetic and must be replaced with the actual fixture inputs. Set
the designated Worker resolver address, business name, one expected A address, approved private IP range, exact response
body, public test CA PEM, and each mandatory-deny hostname/address/port. Use a live
ALB address for the control-plane numeric target. Keep all CA/leaf private keys
outside the source, prepared project, deployment bundle and evidence. The public
CA certificate is the only trust material in `config.ts`.

The deployment owner must establish and retain these positive controls immediately
before each acceptance cohort:

- The Worker host resolves every configured name to the configured address and
  can reach business HTTPS with the test CA and exact `/probe` body.
- The Worker host reaches the private control-plane listener and the configured
  execution and company-client listeners. Arrange disposable listeners and narrow
  fixture security-group rules where needed. An unused port is not a policy deny.
  Metadata TCP 80 must be reachable from the Worker host; a token-required HTTP
  response suffices. Do not capture metadata bodies or credentials.
- The Worker host can query `alternateDNS.query` at `alternateDNS.address` over
  UDP and TCP 53. The defaults use `example.com` at `1.1.1.1`; change them if the
  authorized environment uses a different controlled resolver.
- Installation policy permits only the configured business range and retains
  control-plane, execution, company-client and metadata denies. These are explicit
  deployment fixture inputs; do not create a Product exception for these addresses.

All mandatory-deny names deliberately appear in the finite allowlist, without
private `ipRanges`. The business name alone carries a private IP range. Supplying
an explicit range overlapping a mandatory deny is rejected at deployment, rather
than becoming a runnable bypass test. Ordinary DNS can resolve management names;
finite DNS must return no A answer for their denied addresses. Both runtimes must
fail their numeric management TCP and allowed-name TLS probes. Metadata is tested
only through DNS observation and numeric TCP 80; no metadata TLS 443 claim is made.

### Disposable HTTPS endpoint

On a private test machine, generate short-lived synthetic certificates, replacing
`business.private.example` with the configured business name. Use a new private
working directory with `umask 077`; retain keys only for the authorized test.

```sh
openssl req -x509 -newkey rsa:2048 -nodes -days 2 \
  -subj /CN=helmr-private-test-ca -addext basicConstraints=critical,CA:TRUE \
  -keyout ca.key -out ca.pem
openssl req -new -newkey rsa:2048 -nodes \
  -subj /CN=business.private.example -keyout canary.key -out canary.csr
cat > canary.ext <<'EXT'
basicConstraints=critical,CA:FALSE
subjectAltName=DNS:business.private.example
keyUsage=critical,digitalSignature,keyEncipherment
extendedKeyUsage=serverAuth
EXT
openssl x509 -req -in canary.csr -CA ca.pem -CAkey ca.key -CAcreateserial \
  -days 2 -extfile canary.ext -out canary.pem
```

Copy only the leaf certificate, leaf key and `serve-canary.py` to the private
business endpoint using the environment's authorized transport. Run it as a
bounded native operation; TCP 443 requires the host's normal bind permission:

```sh
sudo systemd-run --unit helmr-private-canary --collect \
  --property User=ubuntu \
  --property AmbientCapabilities=CAP_NET_BIND_SERVICE \
  --property CapabilityBoundingSet=CAP_NET_BIND_SERVICE \
  --property NoNewPrivileges=yes \
  /usr/bin/python3 -I -B /absolute/private/serve-canary.py \
  --cert /absolute/private/canary.pem --key /absolute/private/canary.key
```

Use absolute paths readable by the unprivileged `ubuntu` user, with the leaf key
restricted to that user. This transient native unit grants only the capability
needed to bind port 443; it does not run the listener as root. Stop the exact unit
during cleanup with `sudo systemctl stop helmr-private-canary.service`.

The server returns precisely `helmr-private-business-canary\n` on `/probe` and
contains no real data. The same disposable HTTPS listener can supply execution
and company-client positive controls; its certificate identity is immaterial to
numeric TCP denial probes. The business positive control requires the correct
certificate and name. From the Worker host, require a valid certificate and exact
body, without `-k`:

```sh
curl --fail --silent --show-error --cacert ca.pem \
  https://business.private.example/probe
```

Delete the test listeners and keys during the environment owner's ordinary
cleanup. The driver owns only its Sessions and Computers.

## Prepare and execute

Use the selected Product checkout's normal local SDK packing path:

```sh
nix develop -c python3 tests/e2e/prepare_project.py /private/private-policy-project \
  --fixtures cases/private-execution-policy
helmr deploy /private/private-policy-project
```

Preparation executes the probes during deployment, so endpoints, private DNS,
installation policy and test CA must already be in place. Keep the exact Product
artifact source identity separate from these test-only source additions. Retain
the fixture diff, configured project, published bundle digest and emitted
`configurationDigest` with the deployment owner's evidence.

Run `ordinary`, `finite`, and `blocked` serially with separate new evidence
directories and the usual `HELMR_API_URL`/`HELMR_API_KEY`. The key requires Computer
create/execute/Hibernate/Wake/delete, Session creation/continuation/cancellation
and definition read permissions. One VM slot suffices; finish cleanup before the
next invocation. `HELMR_EXPECTED_BUNDLE_DIGEST` is required and binds the driver to the actual
promoted fixture deployment. Before permitting probes, the driver also resolves
the actual Computer and Session deployment pins and rejects a promotion race
that selected another bundle. `HELMR_PRIVATE_POLICY_WORKER_HOST_ID` is also required:
set it to the source logical Worker Host UUID from the operator's actual allocation
evidence. Default synthetic domains and missing/invalid CA material fail preflight.

```sh
HELMR_PRIVATE_POLICY_PROBES=1 \
HELMR_PRIVATE_POLICY_SUBJECT=finite \
HELMR_EXPECTED_BUNDLE_DIGEST=sha256:ACTUAL_PROMOTED_BUNDLE_DIGEST \
HELMR_PRIVATE_POLICY_WORKER_HOST_ID=ACTUAL_SOURCE_HOST_UUID \
HELMR_EVIDENCE_DIR=/private/evidence/private-finite-hibernate \
  bun run /private/private-policy-project/cases/private-execution-policy/run.ts
```

Each Turn first emits a ready observation and waits without probing. The driver
writes `positive-control-first-request.json`, or
`positive-control-continued-request.json` after Wake. Observe the resident
Computer's actual current Host and run **every** target listed in that request
from that Host. The business HTTPS control must validate the configured CA, status
200 and exact body; TLS controls must handshake with the exact listed SNI (or none);
DNS controls must resolve the listed query, including `expectedIPv4` when present.
For TCP DNS, require a successful real DNS response, not just an open TCP port.

Write the corresponding `positive-control-first.json` or
`positive-control-continued.json` atomically. Copy every request field verbatim,
then add:

```json
{
  "observedAt": "ACTUAL ISO-8601 UTC OBSERVATION TIME",
  "evidencePath": "/private/path/to/actual-host-controls.json",
  "placement": {
    "evidencePath": "/private/path/to/native-computer-path/result.json",
    "observation": "REPLACE WITH THE COMPLETE computer-path RESULT OBJECT"
  },
  "results": [
    { "target": "COPY ONE COMPLETE TARGET OBJECT FROM THE REQUEST", "success": true }
  ]
}
```

The example above illustrates the extra fields. Run the named `computer-path`
observation after the phase request and include its complete result object in
`placement.observation`, preserving its native evidence directory. The driver
requires the observed Computer, a fresh observation time, and the latest active,
unfenced, unexpired-at-observation lease on the expected Host. It derives and
retains the lease epoch, Worker epoch and allocation (`computer_instance_id`)
from that result. `target` must be the actual object, and `results` must cover every request target exactly once. No placeholder
receipt is accepted. Each observation must follow its phase request and be no
older than two minutes when consumed. Receipt identities must match the attempt,
phase, configuration digest, Computer, Turn, and expected logical Worker Host.
The continued phase expects the original Host for same-host Hibernate/Wake, or the
replacement gate's exact destination Host. Only after validation does the driver
send the native `probe` message permitting guest traffic. Each gate has a ten-minute
bound. IPv6 is separately labeled `guest-disabled-topology` and requires no usable
guest route; its failed dial is not a remote-endpoint enforcement claim.

These receipts and their supplied observations are operator attestations; the
driver validates the projection but does not authenticate database provenance.
Retain independent native evidence mapping the actual Computer/lease to that logical Host and the complete successful
host controls for **both** phases. Receipt validation does not independently
establish those facts. Preparation runs before these runtime gates: the deployment
owner must separately retain positive controls from the actual preparation Host,
bound to the configured digest, preparation build and observation times, before
deployment. Missing preparation proof leaves preparation acceptance incomplete
even if the runtime driver passes. The final result explicitly records
`preparationEnforcementVerified: false`: a standalone driver pass covers the
retained preparation marker and observations, and excludes preparation enforcement
acceptance until the independent actual preparation-host controls are bound to
the deployed bundle, configuration digest, preparation build and observation times.

Each invocation retains the published runtime/preparation policies, Computer pins,
preparation marker, Session nonce/counter and network decisions before and after
Hibernate/Wake. Wake must not fabricate a Turn. Private HTTPS positives require
normal certificate validation and an exact body. Finite and blocked probes reject
numeric TLS without SNI, numeric TLS with an unlisted SNI, and alternate DNS UDP
and TCP paths. Numeric TLS carrying the **allowed** business SNI is valid finite
access and is positively tested; the policy does not promise to reject it.
IPv6 probes require no usable guest default route and a failed numeric connection;
this qualifies the disabled IPv6 path, rather than an IPv6-capable remote endpoint.

Probe errors retain their reason codes; timeouts and refused connections remain
observations. Count a deny as enforcement evidence only alongside the positive
controls and appropriate Worker packet/policy evidence. Hostnames and public CA
material are non-secret; never add real request bodies or keys to these fixtures.

## Operator-controlled replacement

Set `HELMR_PRIVATE_POLICY_LIFECYCLE=replacement` to hold the Computer parked before
Wake. Wait for `parked.json`. The operator then uses the existing provider lifecycle
to drain and replace the source Worker, verifies the exact checkpoint, Session
membership, disk save, source fencing/reclamation and replacement readiness, and
writes `worker-replaced.json` atomically in the same evidence directory:

```json
{
  "marker": "COPY marker FROM parked.json",
  "computerId": "COPY computerId FROM parked.json",
  "sourceHostId": "OBSERVED source logical Host UUID",
  "replacementHostId": "OBSERVED replacement logical Host UUID",
  "evidencePath": "/private/path/to/operator-lineage-evidence"
}
```

The driver waits up to 20 minutes for this exact attempt's marker, then requests
Wake and repeats the full policy probe and retained Session assertions. A timeout
never authorizes killing an undrained Worker. The marker is operator attestation,
not independent lineage verification. The result explicitly records
`physicalLineageVerified: false`. The dedicated-host observation helpers require a
local profile API and cannot prove an AWS provider boundary; retain real provider,
checkpoint and destination-lease evidence separately.

For warm/cold/zero qualification, arrange each state through the existing Capacity
interface and retain the selected ready preparation leaf, actual boot-seed build
ID and Worker adoption/startup evidence. Repeat the three subjects and both
lifecycle modes for the claimed states. This driver changes no preparation policy
and makes no warm-adoption claim from a ready leaf alone. It also does not prove
physical cross-host replacement from Session state alone.

## Source checks

```sh
nix develop -c scripts/check-e2e.sh
nix develop -c node --test tests/e2e/cases/private-execution-policy/probe.test.mjs
```

The TLS helper test establishes certificate validation and ensures reachable TLS
or HTTP error responses cannot be mistaken for policy denial. Compiler analysis,
typechecks and local tests execute no real preparation/guest workload.
