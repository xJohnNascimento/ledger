# Ledger

A double-entry accounting service whose guarantees can be checked: through its API, through database constraints that refuse bad data even from a buggy routine, through concurrency tests against real PostgreSQL, and through an independent rebuild of every balance from the journal.

It knows nothing about FX, providers or workflows. [Corridor](https://github.com/xJohnNascimento/corridor), an FX payments product, is its first client.

**Status:** The accounting model, reservations, API, independent audit, operator controls and Terraform sandbox are built and tested. Each [release](https://github.com/xJohnNascimento/ledger/releases) publishes a signed image to `ghcr.io/xjohnnascimento/ledger` and tags the API module. The benchmark is next, as the stretch task in [`docs/plan.md`](docs/plan.md).

## What it guarantees

- Every entry balances per currency, and every balance equals the sum of its postings.
- An enforced account never goes below its available amount, under any concurrency.
- A command reference executes at most one body; a retry gets the stored answer, including a stored refusal.
- Reservations protect funds on several accounts at once, and settle or release with their final journal in one transaction.
- The application's database role cannot insert, update or delete any table: every change goes through reviewed routines.

## API

Connect (gRPC and HTTP/JSON): accounts, journals, reservations, commands, balances, postings and an audit export. The API is its own Go module, `api/`, released with `api/vX.Y.Z` tags. Fields and errors: [design, section 4](docs/design.md#4-api).

## Run it

To see it working inside the whole system, with payments, failures and the independent verifier, use [Corridor's quick start](https://github.com/xJohnNascimento/corridor#quick-start). On its own:

```bash
make up
```

The first run writes random secrets for the four API clients to `.env` (ignored by git); the clients and their scopes are in [`deploy/clients.yaml`](deploy/clients.yaml).

| Command | Does |
| --- | --- |
| `make test` | Unit, integration, concurrency and property tests; integration tests start PostgreSQL in a container |
| `docker compose exec ledger /ledger audit corridor` | Rebuilds every balance from the journal and reports any difference |
| `make lint`, `make proto-check`, `make tf-validate` | Static checks, as CI runs them |

## Try it by hand

With `make up` running, the API answers on `localhost:9090`. It is Connect, so plain `curl` with JSON works. Amounts are integers in minor units, and each client may only do what its scopes allow ([`deploy/clients.yaml`](deploy/clients.yaml)). Load the local secrets from the `ledger` directory:

```bash
set -a; . ./.env; set +a
```

Create (or find) two accounts with the seed client. The same request always returns the same account:

```bash
curl -s localhost:9090/ledger.v1.LedgerService/EnsureAccount -H "Authorization: Bearer $LEDGER_SECRET_SEED" -H "Content-Type: application/json" -d '{"ledgerId":"corridor","ownerKind":"OWNER_KIND_PROVIDER","ownerId":"ditto","purpose":"cash","assetCode":"CAD"}'
```

```bash
curl -s localhost:9090/ledger.v1.LedgerService/EnsureAccount -H "Authorization: Bearer $LEDGER_SECRET_SEED" -H "Content-Type: application/json" -d '{"ledgerId":"corridor","ownerKind":"OWNER_KIND_HOUSE","ownerId":"house","purpose":"funding_equity","assetCode":"CAD"}'
```

Post a balanced journal between them. Sending it again with the same command reference returns the same entry instead of posting twice:

```bash
curl -s localhost:9090/ledger.v1.LedgerService/PostJournal -H "Authorization: Bearer $LEDGER_SECRET_SEED" -H "Content-Type: application/json" -d '{"command":{"namespace":"seed","reference":"fund-1"},"ledgerId":"corridor","journal":{"kind":"treasury","productReference":"fund-1","lines":[{"accountId":"CASH_ACCOUNT_ID","side":"SIDE_DEBIT","assetCode":"CAD","amount":"100000"},{"accountId":"EQUITY_ACCOUNT_ID","side":"SIDE_CREDIT","assetCode":"CAD","amount":"100000"}]}}'
```

Read the balance with the operator client, which has the read scope, then rebuild every balance from the journal:

```bash
curl -s localhost:9090/ledger.v1.LedgerService/GetBalance -H "Authorization: Bearer $LEDGER_SECRET_OPERATOR" -H "Content-Type: application/json" -d '{"accountId":"CASH_ACCOUNT_ID"}'
```

```bash
docker compose exec ledger /ledger audit corridor
```

## Releases

Every pull request carries one label that says what merging it releases: `release: major`, `release: minor`, `release: patch` or `release: none`. Once it merges and CI passes on `main`, the next version is tagged twice, `vX.Y.Z` for the service and `api/vX.Y.Z` for the Go API module, with GitHub release notes and a signed multi-architecture image on GitHub Container Registry. A change that breaks the API compared with the last release must be labelled `release: major`, or `release: minor` before 1.0.

A tag pushed by hand builds, signs and publishes the image after CI passes, and nothing more: it gets no `api/` tag and no GitHub release.

## Documentation

| Document | Contents |
| --- | --- |
| [`docs/design.md`](docs/design.md) | Accounting model, reservations, API, data model, transaction protocol, tests, [code layout](docs/design.md#11-code-layout) |
| [`docs/plan.md`](docs/plan.md) | Build tasks L0–L7 |
| [`docs/adr/`](docs/adr/README.md) | Decisions: reservations, the single-writer design |
| [`infra/README.md`](infra/README.md) | Terraform for an optional AWS sandbox |

## License

[MIT](LICENSE)
