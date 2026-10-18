# Native SDK guest qualification

Build `tests/build/native-execution-package.sh` with `HELMR_NATIVE_SDK_PROOF=1`
on the authorized Linux build host, then select the same flag for the ordinary
`tests/build/native-execution-kvm.sh` invocation. The source receipt binds this
image variant. Native packages and OS tools are installed during image creation;
the guest retains the normal deny-all external network policy.

The cases execute real native SDKs with mock models inside an ordinary Computer
command. A nested network namespace exposes only loopback, and Tini reaps orphan
children in the test PID namespace. No provider credentials are passed. Each case
has fresh native storage, a 55-second deadline with five seconds of kill grace,
and must exit successfully.

The driver first runs a serial startup diagnostic in the same image and network
isolation. Adjacent first/repeat passes for Bun and each SDK time package reads, imports
and CLI version calls, with
120 seconds per step and eight minutes overall. Step failures are observations;
the behavior cases retain their own assertions and deadlines. The diagnostic
must finish and reconcile before the cases start. Separate startup receipts and
logs preserve those results. Because this reads package files before the cases,
the resulting behavior qualification does not establish cold-start latency.

The suite covers retained native operations, cooperative interruption, native
owner shutdown or loss, history reconstruction, and fresh input afterward.
Pending input retirement is explicit for Pi (clearQueue), Codex (queue deletion),
and OpenCode (composed inbox cancellation). Claude input queued before interruption
was observed reaching the model afterward; interruption alone does not retire it.
The Claude close/reopen cases separately assert inert reconstruction and fresh
input success. The selected Codex baseline disables native Goals; its background
shell survives turn interruption until explicit backgroundTerminals/clean.
OpenCode recovery deliberately retires synthetic recovery notifications as well
as user queue entries; the cases assert this selected behavior. Native owner
death and external descendant containment are separate observations.

These tests qualify SDK behavior on the guest Linux kernel. They do not prove
four-SDK Helmr Turn adaptation, same-process RAM checkpoint restoration, live
provider compatibility, or Protected Secrets WebSocket transport. The surrounding
native execution fixture retains its separate existing runtime assertions.
