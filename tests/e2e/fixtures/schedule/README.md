# Schedule fixture

This isolated deployment tests declarative Schedule admission, firing and archival.
Promote it only for the Schedule case, then restore the exact prior ordinary bundle
and archive the omitted Schedule. Do not leave background work in a retained host.

Use `tests/e2e/prepare_project.py OUTPUT --fixtures fixtures/schedule/tasks` from
the Product root to prepare local SDK archives without publication. Managed release
validation installs its selected signed SDK/proto bytes before building the fixture.
The provider-owned case owns deployment replacement, assertions and restoration.
