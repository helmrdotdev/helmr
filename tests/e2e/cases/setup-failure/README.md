# Initialization failure cleanup

Prepare `cases/setup-failure cases/slack-conversation` with the ordinary E2E
project preparer. Use a dedicated runtime host with exactly one execution slot,
in an Environment with no other eligible host, then deploy and create a
`setup-failure` Session with initial text input and its own new Computer.

Observe through the ordinary Session API that initialization records a local
failure hold, the process reaches `stopped`, and the initial Turn never starts.
The retained failure must not leave the process indefinitely `stopping`.
Where host evidence is available, verify `failure_recorded_at` and `fenced_at`
on that exact process and exactly one local failure hold.

Cancel the failed Session, delete its Computer through the ordinary API, and
create `verification-beta` without an existing Computer, with text input
`case:silent marker:after_setup_failure`. Require its Turn to complete and its
authorized Turn result read to contain that marker. Verify its Computer ID differs
from the deleted one. The Environment’s sole eligible host establishes placement;
read-only allocation evidence may additionally confirm the exact host identity. This makes a retained
allocation observable. Do not repair database rows or fabricate a stop receipt.
Record cleanup separately from the initial failure, then close the healthy
Session and delete its Computer.

A paid-provider sample is separate acceptance: deploy `examples/opencode-assistant`
with its documented Secret setup to this same host, then require a plain-language
reply from another fresh Computer. The synthetic probe does not establish that
provider or sample integration.
