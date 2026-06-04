# Full Rootfs Runtime Fixture

This fixture is used by the gated CLI integration tests for the full-rootfs runtime contract.

- `contract` verifies Debian slim task execution after `apt-get install -y curl`, `/usr/bin` visibility, image env propagation, image `PATH` precedence over the runtime default, image `WorkingDir`, and workspace source-tree precedence over image files.
- `agent-user` verifies named user launch with the same image env contract and non-root edits to existing checkout paths.
- `default-root` and `default-workdir-path` verify the root user, workspace cwd, and default `PATH` behavior when `.user()`, `.workdir()`, and `.env("PATH", ...)` are not specified.
- `failure-boundary` writes both workspace and rootfs-side files before failing so tests can verify failure-path filesystem boundaries.
- `approval` verifies the `ctx.wait.human` request/response relay.
- `impl` is the source/image fixture. Its image copies only `source.file("package.json")`, runs an install-like image layer with a cache mount, and uses the workspace mounted at `/workspace`. The gated E2E harness uses it to verify fresh build, same dependency input cache hit, code-only source changes, dependency input image-key changes, and 10-way singleflight.
- `alpine-starts` and `distroless-starts` verify that the product adapter starts with the Node runtime provided by Alpine and shell-less/distroless-style rootfs images.

The fixture intentionally writes `local-tool-workspace-write.txt` under the Helmr workspace and never writes `hosted-agent-remote-write.txt`. Hosted agent tools run in their provider's remote filesystem; those files are separate from the Helmr workspace unless task code explicitly copies artifacts back into the mounted workspace.
