# Console

Console dashboard for managing a self-hosted Helmr control plane.

## Local development

Run the dashboard against a local control server and seeded Postgres database:

```sh
make dev
```

Open `http://127.0.0.1:3000/dev/login` to create a local owner session. The
stack stores its disposable Postgres data under `.helmr-dev/` and resets that
managed database on each start so the seeded dashboard state matches the
current schema.
