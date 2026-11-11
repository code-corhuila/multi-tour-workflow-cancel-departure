# multi-tour-workflow-cancel-departure

> Workflow saga cancel-departure (operations-cost), with no undo: cancelled is irreversible

Part of the **Multi Tour** distributed system — team `multi-tour`, Grupo 2.
Governance and documentation live in [`multi-tour-docs`](https://github.com/code-corhuila/multi-tour-docs).

## What it is

The saga that cancels a departure and handles its reservations (ADR-004 D3, story HU-WFL-004): `operations-cost` cancels the departure; `commercial-reservation` leaves without effect the proposals that point to it and opens the rescheduling episode of each reservation, which marks it, releases its seat and freezes its deadlines (its refund decision and its notice follow from the episode). A cancellation cannot be undone, so there is no compensation: the saga repeats what failed with the same keys and, if something keeps failing, ends `FAILED` naming the reservations left, for a person to decide. The Tenant Administrator starts it; job 3 starts it for a tenant that is inactive. Its state is persisted after every step and every reservation.

## How to start it

Run the migrator once per environment (see `deploy/compose.yml`), then `docker compose -f deploy/compose.yml up workflow-cancel-departure`, with the variables of `.env.example` (the compose file maps the secrets from `env/<environment>.env` of `multi-tour-infra-postgres`) and the `SERVICE_TOKEN` issued by the security service. The service listens on 8090 inside the platform network, under the name `multi-tour-workflow-cancel-departure`, and publishes no port. Entry through the gateway: `POST /api/v1/sagas/cancel-departure` and `GET /api/v1/sagas/cancel-departure/{id}` (see `openapi/openapi.yaml`); job 3 calls the internal route directly, which the gateway never routes. The request that starts a saga has a budget of 60 seconds: what is not done by then is saved as `RUNNING` and goes on in the resumption, which takes every minute the sagas that have not moved for two minutes.

## Where the data is

In PostgreSQL, schema `workflow_cancel_departure` of the single instance (ADR-005 E4); its migrations are in `db/`, with the control table `databasechangelog_workflow_cancel_departure`.

## How to test it

`gofmt -l .` must print nothing, then `go vet ./...` and `go test ./...` (Go 1.26). The tests of the store need `TEST_DATABASE_URL` (a PostgreSQL where the test can create databases) and are skipped without it; the CI sets it. `sh db/tests/db-ci.sh` builds the schema from an empty PostgreSQL, reruns it, rolls it back and rebuilds it (Docker is the only requirement). The same checks, and the build of the image, run in `ci.yml` on pull requests to `develop` and `qa`.

## What is missing

The routes that the saga calls do not exist yet: the cancellation of a departure in `operations-cost`, and the list of the reservations of a departure for a service in `commercial-reservation` (plan D-02 of the spec in `multi-tour-docs`); the two routes of the episodes are in its contract. The saga is tested against doubles that follow them, and has not been run end to end. The route of the saga in the gateway for the internal start and the include in the root composition belong to the shared repositories.

## Branching

Three permanent branches. **None of them accepts a direct commit** — you enter through a child
branch and leave through a Pull Request.

```
develop  <--PR--  feat/... fix/... chore/...
qa       <--PR--  qa/...
main     <--PR--  release/...  hotfix/...
```

Promotion happens **by re-application** (`git cherry-pick -x`), never by merging one permanent
branch into another: `merge develop -> qa` and `merge qa -> main` do not exist in this model.

`main` requires **1 approval from `ariel5253`**. On `develop` and `qa` the team sets its own review
rule.

Full policy: `00-governance/branching-policy.md` in `multi-tour-docs`.
