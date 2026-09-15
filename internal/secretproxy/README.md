# Host mediation ownership

The Firecracker network binding owns one Proxy per runtime. Accepted original
destination sockets stay in the listener namespace. Outbound sockets originate
in the host namespace, use the original numeric public IPv4 destination, and
verify the exact protected service hostname with ordinary TLS. Shared IPs do not
convey credential authority. Every HTTP request checks its authority and resolves
header selectors against the immutable live runtime binding.

The proxy admits 256 captured workers before starting goroutines or dialing.
Each worker owns an accepted socket, at most one speculative/opaque upstream,
ClientHello recording (64KiB), and its read/copy goroutines. Other-port kernel
forwarding and loopback do not consume this admission. Overflow closes the
accepted socket without creating a worker or upstream; no user-space queue.
A speculative socket is handed to at most one protected request transport after
classification, through its existing peek reader. This avoids a discarded extra
connection while preserving immediate server-first protocols. No authenticated
upstream connection is pooled between requests.

A separate admission of 256 complete protected requests bounds multiplexed
forwarding work and transports. Overflow returns a fixed 503 before parsing
selectors or calling Resolve. The existing 64-call Resolve window is held only
through control-plane resolution; its waiting requests remain cancellable and
are included in the 256 admitted requests. Streaming holds request admission.
Close cancels the root context, closes all owned sockets, then joins workers and
handlers. No admission, socket, or resolved value survives runtime fencing.

HTTP/2 uses the maintained Go server/transport. Server concurrency is four
streams per connection; frame size is 64KiB, receive windows are 256KiB per
connection and 64KiB per stream on both sides. These bound work before handler
admission, including HEADERS, which DATA flow control cannot bound. Clients may
queue or open more connections. There is no separate mediated-connection limit,
byte-accounting pool, persisted quota, scheduler, or per-stream response timeout.

## Resource envelope and qualification

With N=256, captured plus speculative/opaque sockets are at most 2N. Admitted
request transports add at most N current upstream sockets; the listener and
transient accept/dial/transport cleanup also consume descriptors. Recorded
ClientHello bytes are at most 16MiB. These are allocation ownership bounds, not
hard process-wide RSS, CPU, or kernel socket-memory isolation.

Do not estimate HTTP/2 memory using only its body window: active stream header
allowances alone are N*4*64KiB=64MiB. In pinned Go1.27.1, early stream resets may
also queue up to 4*streams+1 handlers per connection before the library closes an
abusive connection, so active-plus-pending header allowances can reach 336MiB,
before map/string/allocation overhead. Guest body connection windows add up to
64MiB; upstream windows include Go's initial 65535 bytes in addition to the
configured 256KiB (about80MiB over256 transports). TLS/parser/frame buffers,
rewritten request/response headers, copy buffers, goroutine stacks and kernel TCP
buffers are additional. These maxima describe components, not a measured or
strict combined RSS ceiling. Protocol retries can overlap transport cleanup.

Worker NetworkCapacity is derived from WorkerExecutionSlots and bounds runtime
network owners. Multiply runtime envelopes by configured slots when qualifying
worker headroom and effective descriptor limits. This change does not reserve
process-wide file descriptors, change worker capacity or host limits, or claim
multi-runtime adversarial load qualification on release hardware.

Portable race tests cover admission overload, partial ClientHello, cancellation,
release, runtime isolation, more than64 simultaneous ordinary TLS streams, and
H1/H2 streaming. Linux namespace tests exercise real rendered TPROXY policy,
preflight isolation, redirects, drift and stale TCP fencing. Neither test class
is proof of a Firecracker image boot/restore with actual guest workloads.
