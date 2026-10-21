# Cloud agent guidance

Follow the workspace `../AGENTS.md`. Use this file for Cloud-specific context; the README is only an operator entry point.

- `cmd/energylens-cloud` is the Go binary for API, worker, migrations, and operator commands. API and worker are separate production roles; `all` is for development only.
- Cloud serves owner and verified Hub routes on one TLS listener. Owner identity comes from a verified bearer session; Hub-only routes require a current client certificate, durable registration, and ownership epoch. Do not trust client-supplied identity headers.
- Keep the application and DDL migration database roles separate. Add numbered, additive migrations; do not change an applied migration or run DDL while serving.
- Datawiz request production belongs to the worker. Datawiz processes those JSON files and publishes source-backed observations. A mobile catalog miss is not a final source miss.
- Local auth is a separately gated development path; keep production authentication configured with real credentials. Never compile or print passwords, database URLs, signing keys, or certificate private keys.
- Before a shared Hub or Mobile acceptance, verify the current authenticated home, owned-Hub connection hint, certificate pin, and authorization from live Cloud responses.
- Detailed operator procedures, including account adoption and Hub provisioning, are in `ENGINEERING_REFERENCE.md`; route and configuration truth is in current code and `.env-example`.
