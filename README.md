# EnergyLens Cloud

The Go service provides the EnergyLens account, home, appliance, and energy APIs.
Its worker prepares Datawiz requests and metered summaries.
The same binary runs API, worker, migration, and operator commands.

## Setup for development, testing, building, and deployment

Install Go and PostgreSQL. Copy `.env-example` to a private mode-`0600` `.env-local`, set the required TLS and database values, and use separate application and migration database roles. The sibling [launcher guide](../energylens-dev-bash-scripts/README.md) covers private role files. Detailed [Cloud operations](ENGINEERING_REFERENCE.md) include account adoption, Hub provisioning, key rotation, and rollback.

## Build commands and scripts

```sh
mkdir -p build
go build -o build/energylens-cloud ./cmd/energylens-cloud
```

## Test commands and scripts

```sh
go test ./...
```

Set `ENERGYLENS_CLOUD_TEST_DATABASE_URL` to include PostgreSQL integration tests. Without it, database tests skip. Use a disposable database with schema and migration privileges.

## Deployment commands and scripts

From `../energylens-dev-bash-scripts`, check each private role file, apply migrations with the DDL role, then start API and worker separately:

```sh
./run-energylens-cloud.sh migrate /private/path/cloud-migrate.env --check
./run-energylens-cloud.sh api /private/path/cloud-api.env --check
./run-energylens-cloud.sh worker /private/path/cloud-worker.env --check
./run-energylens-cloud.sh migrate /private/path/cloud-migrate.env
./run-energylens-cloud.sh api /private/path/cloud-api.env
./run-energylens-cloud.sh worker /private/path/cloud-worker.env
```

## Troubleshooting

Check `/readyz` and certificate trust against the configured HTTPS origin. A failed `--check` points to a private configuration or key problem; a migration failure calls for checking the DDL role and migration ledger. Inspect service logs without printing secret environment files.
