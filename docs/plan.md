# Ledger: work plan

The ledger's build tasks, in order. Each task is done when its code and tests are merged with CI green, its "done when" is shown, and the design is updated in the same pull request. How these tasks fit with Corridor and Ditto, and which tasks Corridor waits for, is in Corridor's [cross-repository plan](https://github.com/xJohnNascimento/corridor/blob/main/docs/plan.md).

**Status:** L0–L6 are done. Versions are on the [Releases page](https://github.com/xJohnNascimento/ledger/releases), each with a signed image on `ghcr.io/xjohnnascimento/ledger` and a tag of the Go API module. L7, the benchmark, is a stretch task and not started.

| Id | Task | Done when | Depends on |
| --- | --- | --- | --- |
| L0 | Repository and toolchain: root module and the `api` module, with the full API contract (`api/proto/ledger/v1/ledger.proto`) linted and generated; Makefile; `compose.yaml` (PostgreSQL 17 and the ledger); GitHub Actions with lint, unit and integration tests, `buf lint`, `buf breaking` against the last `api/v*` tag, and an image build; on a release tag, push the image to GitHub Container Registry with an SBOM and a signature; README stub | A pull request runs green; a release tag publishes an image; `make up` reports the ledger healthy | none |
| L1 | Money types, schema, domains, roles, the deferred balance trigger, `ensure_account` | Domain and raw-SQL invariant tests pass; `ledger_app` cannot insert, update or delete any table | L0 |
| L2 | Command claim protocol, `PostJournal`, scope checks (namespaces, purposes), the Connect handlers for the contract, and error mapping | Concurrent duplicate, concurrent debit and lost-reply tests pass; an out-of-scope namespace is refused; released with the API module for Corridor's customer setup (C1) | L1 |
| L3 | `ReserveFunds`, `CommitReservation`, `ReleaseReservation` with return journals, admission attempts | Finalization races, debit-normal funds and attempt-adoption tests pass; released with the API module for Corridor's walking skeleton (P1, C2) | L2 |
| L4 | `GetBalance`, `ListPostings`, `ExportAuditSnapshot` with the `audit` scope, `ledger audit` | The independent rebuild matches after mixed workloads; an interrupted export ends INCOMPLETE; a client without `audit` cannot export; released with the API module for Corridor's verifier (P2) | L3 |
| L5 | Operator scope: account controls and corrections; cross-ledger and role tests | A blocked account refuses new reservations but settles an existing one; Corridor's money client cannot export, change controls or correct; released with the API module for Corridor's v2 decisions (C5) | L4 |
| L6 | Terraform for the sandbox ([`infra/README.md`](../infra/README.md)) | `terraform validate` and `tflint` pass in CI without cloud credentials | L0 |
| L7 | Stretch: `ledger bench` and its results in the README | Reproducible results include the hot-account ceiling and its explanation | L5 |

Corridor's scenarios exercise the ledger end to end. A ledger release is ready for Corridor when its own tests pass and Corridor's scenarios pass against its image.
