# Helmr AWS Examples

This directory contains customer-facing infrastructure examples that vary by operating model.
Deployable Helmr-owned environments live in `../stacks`.

- `quickstart` is the low-cost evaluation and startup path. It favors a small footprint and can use
  a generated CloudFront URL before the customer has a domain ready.
- `standard` is the recommended single-account production baseline. It keeps compute in private
  subnets, uses NAT for outbound access, and enables stronger database durability defaults.
Use the examples as starting points in the customer's own infrastructure repository. Commit that
repository's generated provider lock file and backend configuration there rather than in Helmr.
