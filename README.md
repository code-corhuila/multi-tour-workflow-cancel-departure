# multi-tour-workflow-cancel-departure

> Workflow saga cancel-departure (operations-cost), with no undo: cancelled is irreversible

Part of the **Multi Tour** distributed system — team `multi-tour`, Grupo 2.
Governance and documentation live in [`multi-tour-docs`](https://github.com/code-corhuila/multi-tour-docs).

## What it is

The saga that cancels a departure and handles its reservations (ADR-004 D3, story HU-WFL-004): `operations-cost` cancels the departure; `commercial-reservation` leaves without effect the proposals that point to it and opens the rescheduling episode of each reservation, which marks it, releases its seat and freezes its deadlines (its refund decision and its notice follow from the episode). A cancellation cannot be undone, so there is no compensation: the saga repeats what failed with the same keys and, if something keeps failing, ends `FAILED` naming the reservations left, for a person to decide. The Tenant Administrator starts it; job 3 starts it for a tenant that is inactive. Its state is persisted after every step and every reservation.

## How to start it

Not built yet: this repository holds the skeleton and the contract of its routes (`openapi/openapi.yaml`). The image, the composition and the instructions to run it come with their tasks (WF4-06 of the spec in `multi-tour-docs`).

## Where the data is

In PostgreSQL, schema `workflow_cancel_departure` of the single instance (ADR-005 E4); its migrations will be in `db/`, with the control table `databasechangelog_workflow_cancel_departure`.

## How to test it

`gofmt -l .` must print nothing, then `go vet ./...` and `go test ./...` (Go 1.26). The tests of the store will need `TEST_DATABASE_URL` and will be skipped without it; the CI sets it.

## What is missing

Everything but the skeleton and the contract. The routes the saga calls do not exist yet either: the cancellation of a departure in `operations-cost`, and the list of the reservations of a departure for a service and the two routes of the episodes in `commercial-reservation` (plan D-02 of the spec in `multi-tour-docs`).

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
