# Ledger: design

A reusable double-entry accounting service. Its guarantees are visible through its API, its database constraints, its concurrency tests and an independent rebuild of every balance from the journal. It serves Corridor without knowing anything about FX, providers or workflows.

Its API contract is [section 4](#4-api). How Corridor uses it (business references, the accounting of one FX payment, the independent verifier) is in Corridor's [integration contract](https://github.com/xJohnNascimento/corridor/blob/main/docs/integration.md).

## 1. Boundary

| The ledger owns | The ledger does not own |
| --- | --- |
| Accounts and their classification, balances, reservations, journal entries, command outcomes, the evidence that explains every balance | Pricing, FX, providers, payment state, retries of external effects, valuation, period close, statements |

- **Deployable:** one `ledger` image with the subcommands `serve`, `migrate`, `audit` and `healthcheck`, published to GitHub Container Registry on every release tag. The API is a separate Go module, `api/`, tagged `api/vX.Y.Z`.
- **Authority:** one PostgreSQL 17 writer. Posting and reservation decisions are ordinary database transactions; Temporal is not involved.
- **Durability:** an acknowledged command is a committed PostgreSQL transaction on the configured storage. The local and sandbox stacks make no regional-loss guarantee.

## 2. Accounting model

### 2.1 Accounts

An account belongs to one ledger and has an immutable classification: owner kind, owner id, purpose, asset, normal side and an `account_key` that distinguishes accounts sharing the rest. The natural key is `(ledger_id, owner_kind, owner_id, purpose, asset_code, account_key)`.

The **purpose catalog** (configuration) fixes each purpose's normal side and whether availability is enforced, so callers cannot misclassify an account:

| Purpose | Owner kind | Normal side | Enforced |
| --- | --- | --- | --- |
| `funds` | CUSTOMER | CREDIT | Yes |
| `cash` | PROVIDER | DEBIT | Yes |
| `clearing` | PAYMENT | CREDIT | Yes |
| `fee_revenue`, `spread_revenue`, `funding_equity` | HOUSE | CREDIT | No |
| `fx_difference` | HOUSE | DEBIT | No |

`EnsureAccount` with an existing natural key returns the existing account. The natural key holds every field the caller supplies and the catalog derives the rest, so a retry can never meet a different classification. An owner kind the purpose does not accept is refused with `ACCOUNT_CLASSIFICATION_CONFLICT`; an unknown ledger or asset is `NOT_FOUND`.

### 2.2 Balances and availability

Balances are stored in the account's normal-side sign:

```
delta(account)  = sum(postings on its normal side) - sum(postings on the opposite side)
available       = posted_balance - reserved_amount + overdraft_limit
```

- An enforced account must have `available >= 0` after every successful money command. For a credit-normal account (customer funds) a debit reduces funds; for a debit-normal account (provider cash) a credit does.
- Pending increases never add availability.
- A debit-positive trial balance negates credit-normal balances before summing.
- `account_balance` is a transactional projection of immutable postings, and `reserved_amount` a transactional projection of active reservation legs. The `audit` command recomputes both independently.

### 2.3 Amounts

- On the wire: canonical decimal integer strings: `0`, or a nonzero digit followed by digits; signed fields allow one leading `-` on a nonzero value. Fractions, exponents, whitespace, leading zeros, `-0` and floats are refused.
- In Go: `math/big` for intermediate sums; final values are range-checked before storage. No floats, no lifetime debit or credit counters.
- In PostgreSQL: unconstrained `numeric` with a zero-scale check and signed 128-bit bounds. A fixed-scale type such as `numeric(39,0)` would round a fractional input before the check, so it is not used. `scale(value) = 0` refuses `1.0` as well as `1.5`, so a stored amount always prints in canonical form; `NaN` and the infinities fail the bounds.

```sql
create domain ledger.signed_amount as numeric
    check (
        scale(value) = 0
        and value >= -170141183460469231731687303715884105728
        and value <=  170141183460469231731687303715884105727
    );

create domain ledger.amount as numeric
    check (
        scale(value) = 0
        and value >= 0
        and value <= 170141183460469231731687303715884105727
    );
```

### 2.4 Journal entries

- 2 to 50 postings, each with a positive amount.
- Debits equal credits **per asset**; a multi-currency entry never balances a converted sum.
- Every posting's account exists, belongs to the entry's ledger and has the posting's asset.
- Entries are immutable. A correction is a new entry with `corrects_entry_id` and a reason, created only with operator scope.

## 3. Reservations

A reservation encumbers funds on one or more enforced accounts, atomically, without posting anything. It lets Corridor protect the customer's CAD and the provider's CAD cash with one call before it asks the provider to trade, and commit the real outcome later in one entry.

`ReserveFunds` records:

- A **logical reservation reference** `(namespace, reference)`, unique per ledger and separate from the command reference of each admission attempt.
- **Legs:** a maximum reduction per enforced account. Duplicate accounts are refused.
- **Scope:** every account a later settlement or return journal may touch, including revenue, clearing and difference accounts. Legs are always in scope.

| Operation | Rule |
| --- | --- |
| `CommitReservation` | Supplies the final balanced journal. Every account in it is in scope; every enforced account with a net reduction is a leg, and the reduction is within its ceiling. The reservation is released and the journal posted in one transaction, so a fill and its difference become visible together |
| `ReleaseReservation` | Releases without settlement. It may carry a balanced return journal under the same scope and ceilings, posted in the same transaction: Corridor uses this to return a failed payout to the customer |

**States:** `ACTIVE → COMMITTED | RELEASED`. Exactly one final transition wins; final records and transition events are immutable. A second finalization under a different command reference is refused with `RESERVATION_FINAL`, which carries the final state, its command and its payload fingerprint, so the caller can tell "already done as I intended" from "done differently".

**Admission attempts.** A reserve refused for funds commits that refusal under its command reference. A later attempt uses a new command reference with the same logical reference. The routine serializes on the logical reference and adopts an existing compatible reservation before checking funds; a refusal leaves no active reservation, and a final reservation is never reopened. This allows liquidity recovery without weakening stable refusals. Compatible means the same fingerprint: a SHA-256 of the legs and the scope, independent of their order. An attempt with other legs or scope is `REFERENCE_CONFLICT`; an attempt on a final reservation stores `RESERVATION_FINAL`.

**No expiry.** Reservations never expire on their own: Corridor's must stay until the provider's outcome is known, however long a review takes. Expiring reservations (card authorizations, for example) are an extension: an expiry worker would finalize them through the same routine and lock order, and a lagging worker could over-reserve but never overstate availability. Partial capture, renewal and amendment are out of scope too.

**Freeze.** A `BLOCKED` account refuses new reservations and any reduction not covered by an existing reservation; credits are still accepted. An existing reservation can still settle within its stored scope, because its external effect may already have happened. A legal hold that must stop even reserved settlement needs its own policy and is out of scope.

## 4. API

Connect-go (gRPC and HTTP/JSON), package `ledger.v1`, defined in `api/ledger/v1/` and checked with `buf lint` and `buf breaking`. Every money mutation carries a command reference:

```
CommandRef { namespace string, reference string }   // namespace must match the caller's allowed prefixes
```

The server computes a canonical SHA-256 hash of the request: operation, ledger, stable ids and normalized fields, with journal lines kept in order; field order in JSON never changes it. A replay with the same reference and hash returns the stored outcome, even if balances have changed since; the same reference with another hash is `REFERENCE_CONFLICT`.

| RPC | Scope | Request | Response |
| --- | --- | --- | --- |
| `EnsureAccount` | provision | `ledger_id, owner_kind, owner_id, purpose, asset_code, account_key` | `account` (id, classification, status, overdraft limit) |
| `PostJournal` | money | `command, ledger_id, kind, product_reference, accounting_date?, lines[] {account_id, side, asset_code, amount}` | `command_id, entry` |
| `ReserveFunds` | money | `command, ledger_id, reservation_ref {namespace, reference}, legs[] {account_id, max_reduction}, scope[] account_id` | `command_id, reservation` |
| `CommitReservation` | money | `command, reservation_id, journal {kind, product_reference, lines[]}` | `command_id, reservation, entry` |
| `ReleaseReservation` | money | `command, reservation_id, return_journal?` | `command_id, reservation, entry?` |
| `GetReservation` | read | `reservation_id`, or `ledger_id + reservation_ref` | `reservation` with legs, scope, state, final command and entry |
| `GetCommand` | read | `ledger_id, command` | `command` (operation, request hash, outcome COMPLETED or REFUSED, reason, result ids (entry, reservation or account), committed_at) |
| `GetBalance` | read | `account_id` | `posted, reserved, available, overdraft_limit, version, last_posting_sequence`, read from the writer |
| `ListPostings` | read | `account_id, after_sequence, limit ≤ 500` (default 100) | postings in account-sequence order, with entry ids; `next_after_sequence` is set when the page is full and 0 at the end |
| `UpdateAccountControls` | operator | `command, account_id, status?, overdraft_limit?` | `account` |
| `PostCorrection` | operator | `PostJournal` fields plus `corrects_entry_id, reason` | `command_id, entry` |
| `ExportAuditSnapshot` | audit | `ledger_id` | a stream: header (schema version, source id, capture time), records, trailer (counts, content hash) |

`GetCommand = NOT_FOUND` does not prove that a concurrent call with that reference cannot still commit; callers resolve an unknown outcome by retrying the same reference with the same body.

`ExportAuditSnapshot` reads one `REPEATABLE READ READ ONLY` snapshot through the `ledger_audit` role, bounds its duration (5 minutes) and volume (5 million records), and ends with `INCOMPLETE` if interrupted. Records come in a fixed order (accounts, balances, entries with their postings, reservations, commands, each by id); the trailer carries the count of each kind and a SHA-256 over the header and records, so a consumer can prove it received everything. It does not claim a cross-service snapshot.

### Errors

Connect codes. Every error that carries a reason has a stable `reason` and typed details; clients branch on the reason, never on the message. Authentication and infrastructure errors carry neither.

| Reason | Code | Stored under the command | When |
| --- | --- | --- | --- |
| `INVALID_AMOUNT`, `INVALID_JOURNAL`, `OUT_OF_SCOPE`, `INVALID_RESERVATION` | invalid_argument | No (deterministic) | Bad amount; unbalanced, wrong asset, wrong ledger or 2–50 legs broken; a journal account outside the reservation scope, or an enforced account reduced without a leg; no legs, duplicate legs, a leg on an account that does not enforce availability, more than 50 legs or 100 scope accounts |
| `REFERENCE_CONFLICT` | already_exists | No | Same reference, different body |
| `ACCOUNT_CLASSIFICATION_CONFLICT` | invalid_argument | No | `EnsureAccount` with an owner kind the purpose does not accept |
| `INSUFFICIENT_FUNDS` | failed_precondition | Yes | Details: account id, available, required |
| `ACCOUNT_BLOCKED` | failed_precondition | Yes | Details: account id |
| `CEILING_EXCEEDED` | failed_precondition | Yes | A commit or return reduces a leg beyond its ceiling. Details: account id, ceiling, required |
| `RESERVATION_FINAL` | failed_precondition | Yes | Details: final state, final command, payload fingerprint |
| `NOT_FOUND` | not_found | No | Unknown id, or one in a ledger the caller cannot see |
| `NAMESPACE_NOT_ALLOWED`, `PURPOSE_NOT_ALLOWED`, `SCOPE_NOT_ALLOWED` | permission_denied | No | A namespace, an account purpose or an RPC outside the caller's scope |
| (no reason) | unauthenticated | No | A missing or unknown bearer secret |
| (infrastructure) | unavailable, aborted, deadline_exceeded | No | Transient: retry with the same reference and body |
| (internal) | internal | No | A failure a retry will not fix, such as a corrupt stored value; logged server-side |

A refusal that depends on mutable state is stored, so a replay returns the same decision. A deterministic validation error is not stored, because the same body always fails the same way.

## 5. Data model

| Table | Columns and constraints | Role |
| --- | --- | --- |
| `asset` | `code` PK, `scale` immutable, `check (scale between 0 and 18)` | Units; seeds BRL, CAD, EUR and USD at scale 2 |
| `ledger` | `id` PK, immutable `legal_entity_key`, `name` | Accounting boundary; seeds one ledger, `corridor` |
| `account` | `id` PK, `ledger_id` FK, `owner_kind`, `owner_id`, `purpose`, `asset_code` FK, `account_key`, `normal_side`, `enforce_available`, `status` (`ACTIVE`, `BLOCKED`); unique natural key; unique `(id, ledger_id)` for composite FKs | Classification and status |
| `account_balance` | `account_id` PK, `enforce_available` with a composite FK to the account's own flag, `posted_balance signed_amount`, `reserved_amount amount`, `overdraft_limit amount`, `version bigint ≥ 0`, `last_posting_sequence bigint ≥ 0`; `check (not enforce_available or posted_balance - reserved_amount + overdraft_limit >= 0)` | The locked availability projection |
| `command` | `id` PK, unique `(ledger_id, namespace, reference)`, `operation`, `request_hash`, `hash_version`, `outcome` (`COMPLETED`, `REFUSED`), `reason` and `detail` for a refusal, the result ids (`entry_id`, `reservation_id`, `account_id`), `committed_at`, `created_xid` | Durable idempotency and stored refusals |
| `journal_entry` | `id` PK, `ledger_id`, unique `command_id`, `kind`, `product_reference`, `recorded_at`, `accounting_date`, `corrects_entry_id` (nullable FK), `reason`, `created_xid` | One accounting event |
| `posting` | PK `(entry_id, line_no)`, `line_no` 1–50, `account_id`, `ledger_id`, `asset_code`, `side`, `amount amount check (amount > 0)`, `account_sequence`; unique `(account_id, account_sequence)`; composite FKs `(entry_id, ledger_id)` and `(account_id, ledger_id, asset_code)` | Immutable legs and the rebuild order |
| `reservation` | `id` PK, `ledger_id`, unique `(ledger_id, namespace, reference)`, `fingerprint` (legs and scope), unique `created_by_command_id`, `state`, `final_command_id`, `final_entry_id`, `final_fingerprint` | Lifecycle |
| `reservation_leg` | PK `(reservation_id, account_id)` and FK to `reservation_scope`, `max_reduction amount check (max_reduction > 0)` | The encumbrance per account; a leg is always in scope |
| `reservation_scope` | PK `(reservation_id, account_id)`, FK to `account` | Allowed settlement accounts, as rows rather than JSON |
| `reservation_event` | `reservation_id`, `version`, `from_state`, `to_state`, `command_id`, `entry_id`, `occurred_at`; unique `(reservation_id, version)` | Append-only transition evidence |

**Invariants the database enforces on its own**, so a routine bug fails closed. Each raises a ledger SQLSTATE:

| SQLSTATE | Refused |
| --- | --- |
| `LD201` | At commit, an entry without 2 to 50 postings (deferred constraint trigger) |
| `LD202` | At commit, an entry whose debits and credits differ in any asset |
| `LD203` | A posting written by any transaction other than the one that created its entry (`created_xid`, stamped by a trigger) |
| `LD204` | Any update or delete of the journal, reservation legs, scope, events and the catalogs; any change to an account other than its status; a command outcome written twice or outside its own transaction; a reservation leaving a final state; `TRUNCATE` on any table |
| `LD205` | At commit, a command without an outcome |

Routines raise their own SQLSTATEs for refusals that are not stored, and the store maps each to an API reason: `LD101` `ACCOUNT_CLASSIFICATION_CONFLICT`, `LD400` `INVALID_AMOUNT`, `LD403` `PURPOSE_NOT_ALLOWED`, `LD404` `NOT_FOUND`, `LD409` `REFERENCE_CONFLICT`, `LD422` `INVALID_JOURNAL`, `LD423` `OUT_OF_SCOPE` and `LD424` `INVALID_RESERVATION`. Stored refusals are not errors: the routine commits them as the command's outcome and returns the command.

Indexes: postings by `(account_id, account_sequence)`; entries by `product_reference`; reservation legs by account. Tables start unpartitioned; no shard tables, archive tier or global posting sequence are needed at this scale.

`account_sequence` is allocated from `account_balance.last_posting_sequence` under the balance lock and increments by the number of new postings, so a rolled-back transaction never consumes a committed position. `version` also changes on reservation-only mutations and is distinct from the posting sequence.

## 6. Transaction protocol

### 6.1 Database authority

Every mutation runs in a reviewed PL/pgSQL routine: `ensure_account`, `post_journal`, `post_correction`, `reserve_funds`, `commit_reservation`, `release_reservation` and `update_account_controls`. Keeping the protocol inside the database means locks are held only for in-database work, never across a network round trip.

| Role | Rights |
| --- | --- |
| `ledger_owner` | Owns the schema; used only by `migrate` |
| `ledger_app` | `EXECUTE` on the routines, `SELECT` on the tables; no `INSERT`, `UPDATE` or `DELETE` anywhere |
| `ledger_audit` | `SELECT` only; used by `audit` and `ExportAuditSnapshot` |

Routines are `SECURITY DEFINER` with `set search_path = ledger, pg_temp`, schema-qualify every object and validate the ledger of every account they touch themselves. The client's purpose scope for `EnsureAccount` is checked in the Go handler; `ensure_account` checks only the catalog (ledger, asset, purpose and owner kind). `EXECUTE` is revoked from `PUBLIC` by default and granted routine by routine. A deferred constraint trigger checks every new entry at commit (2–50 legs, balanced per asset) and composite keys check membership, so a routine bug still fails closed ([section 5](#5-data-model)). Runtime roles cannot alter or disable triggers. The first migration creates the roles and the schema, then runs as `ledger_owner`, so the migrating user (a superuser locally, the RDS master user in the sandbox) never owns a ledger object.

### 6.2 Locking

`READ COMMITTED` with explicit locks; the correctness argument is the lock order, not the isolation level.

1. The Go handler authenticates the caller, checks namespace and purpose scope, parses and bounds the payload, and computes the canonical hash. Nothing is locked yet.
2. In one transaction, the routine inserts the command claim on `(ledger_id, namespace, reference)`. A conflict waits for the winning transaction; the routine then reads the committed claim in a **new statement**: same hash returns the stored outcome, another hash is `REFERENCE_CONFLICT`. It never uses `ON CONFLICT DO NOTHING` with a read in the same statement, whose snapshot cannot see the winner ([PostgreSQL isolation](https://www.postgresql.org/docs/17/transaction-iso.html)).
3. Reservation creation takes `pg_advisory_xact_lock(hashtextextended(ledger || '/' || namespace || '/' || reference, 0))` and then checks for an existing reservation; a hash collision only adds contention, and the unique key still enforces identity. Finalization locks the reservation row and compares any final payload. Both take account locks only afterwards.
4. Lock every affected `account_balance` row in ascending account id: legs, scope accounts touched by the journal, everything. Account controls use the same locks.
5. Under those locks, read classification and controls, validate scope and journal, aggregate each account's net delta, and check ranges and funds. On a commit, and on a release that carries a return journal, the reservation's own legs are subtracted from `reserved_amount` before availability is evaluated; other reservations stay intact.
6. Write the entry, postings with their sequences, balances, reservation state and event, and the command outcome.
7. Commit, and only then answer. A refusal (insufficient funds, blocked account, final reservation) commits its command outcome with no entry or reservation. An infrastructure error rolls everything back and leaves the request retryable.

No durable PENDING claim survives a crash: the claim and its outcome commit together.

Concurrent commit and release take different command claims but the same reservation lock, so one wins. A deadlock in tests means a lock-order bug and fails the build. Serialization failures, deadlocks, lock waits beyond `lock_timeout` (2 s), statements beyond `statement_timeout` (10 s), lost connections and unknown commit results are retried by the caller with the same reference. The audit pool has no statement timeout, because the export streams through long statements; the export bounds itself instead.

## 7. Authorization, controls and corrections

- Only internal services call the ledger; customers never do. Each client sends its secret as `Authorization: Bearer <secret>`. The ledger reads each client's secret from the environment variable its entry names (`.env`, generated with random values by `make up`, locally; Secrets Manager in the AWS sandbox), keeps only its SHA-256 digest and refuses to start if a secret is missing, shorter than 32 characters or shared. A production deployment would replace secrets with workload identity over mutual TLS.
- `clients.yaml` ([`deploy/clients.yaml`](../deploy/clients.yaml)) maps each client to one ledger, its namespace prefixes, its allowed purposes and its scopes (`provision`, `money`, `read`, `audit`, `operator`). A namespace prefix matches itself and anything below it on a dot boundary: `corridor` allows `corridor.reserve-payout` but not `corridorx`. Allowed purposes limit both the accounts a client may create and the accounts its journals may touch. Corridor's worker has `provision`, `money` and `read` for `corridor.*` and the purposes it needs (it creates clearing, customer and house revenue accounts); the seed runner has `provision` and `money`, limited to `seed.*` namespaces and the `funds`, `cash` and `funding_equity` purposes; Corridor's reconciliation and the independent verifier use a separate client with `read` and `audit` only; the operator client alone can change controls and post corrections. Corridor's money client never carries `audit` or `operator`.
- A balanced entry alone is not authorization.
- **Controls.** `UpdateAccountControls` changes an account's status, its overdraft limit or both, under the same balance lock as money commands and with its own command reference. Lowering an enforced account's overdraft below what the account already uses is stored as `INSUFFICIENT_FUNDS`, so availability never turns negative. A `BLOCKED` account still accepts credits and settles its existing reservations ([section 3](#3-reservations)).
- **Corrections.** `PostCorrection` posts an ordinary balanced entry that names the entry it corrects (which must exist in the same ledger) and carries a reason of 1 to 1,024 characters. Nothing is edited or deleted; the original entry and its correction both stay in the journal.
- Resources in another ledger answer `NOT_FOUND`: a client bound to another ledger cannot read, reserve, settle, post on or export anything outside its own, and tests cover each RPC.
- Command identities and journal evidence live for the dataset's lifetime; there is no automatic purge.

## 8. Audit and rebuild

`ledger audit` reads the same stream `ExportAuditSnapshot` sends, through the `ledger_audit` role, and rebuilds the ledger from it without trusting a stored balance, a routine or a constraint. It first checks the snapshot itself (a header, a `COMPLETE` trailer, matching counts and content hash), then:

- the fold of each account's postings equals `posted_balance`, and sequences are contiguous (V2);
- the sum of active reservation legs per account equals `reserved_amount` (V3);
- every entry balances per asset with valid membership (V1);
- every command outcome links to the entry or reservation it created.

It prints a report and exits non-zero on any break. A test bypasses the routines as a superuser with triggers off, edits a posting and a reserved amount, and the audit reports V1, V2 and V3. Corridor's independent verifier runs the same checks from the exported snapshot.

## 9. Tests

| Test | Required result |
| --- | --- |
| Two concurrent debits of 80 against available 100 | One succeeds, one is refused and stored; available 20 |
| Same reference from many callers | One accounting effect, one result; a changed body conflicts |
| Reply lost after commit | The same-reference retry returns the original entry, even after balances changed |
| Unbalanced cross-asset entry | Whole transaction refused; no partial mutation |
| Commit vs release | At most one finalization and one settlement or return entry |
| Existing reservation plus another debit | Other reservations stay protected |
| Funds arrive after a stored reserve refusal | The same command still refuses; the next attempt creates or adopts one reservation |
| Debit-normal provider cash | Concurrent reservations cannot spend the same prefunding twice |
| Fill plus difference | One atomic journal; no moment with an overstated provider balance |
| Direct SQL write or journal edit as `ledger_app` | Refused by the database |
| Rebuild | The independent fold equals every balance and reserved amount after mixed workloads |
| Integer boundaries | Fractional, overflowing, non-canonical and mixed-asset input fails; wide intermediate sums stay exact |

Locking and constraint tests run against real PostgreSQL (testcontainers); a mocked store cannot verify this protocol. Property tests (`rapid`) run random operation histories against a small independent model and replay the failing seed. Failure tests cut real boundaries (killed connections), not only injected errors.

## 10. Performance demonstration

Stretch goal, after the ledger is otherwise done. `ledger bench` runs three workloads: independent customers, one hot customer, and a shared provider or house account. It records the operation mix, account skew, lock waits, transaction duration, throughput, p50/p95/p99, errors, CPU and storage settings, plus an overload run that shows bounded API concurrency and retry behaviour.

The first ceiling is expected on a shared provider or fee account. The README explains why more API replicas do not remove a serialized row, and why spendable balances are not pre-sharded: sharding non-enforced reporting accounts, batching postings or splitting ledgers comes only after a measured bottleneck, and provider cash needs an explicit allocation model before it can be sharded.

## 11. Code layout

```
ledger/
├── api/                 its own Go module: the protobuf (proto/ledger/v1) and the generated Go
├── buf.yaml, buf.gen.yaml
├── cmd/ledger/           serve, migrate, audit, healthcheck
├── internal/
│   ├── amount/           canonical parsing, range checks, math/big helpers
│   ├── journal/          request validation and canonical hashing
│   ├── store/            routine calls and read queries (pgx)
│   ├── server/           Connect handlers, auth and scope interceptor, error mapping;
│   │                     integration, concurrency and property tests against real PostgreSQL
│   ├── export/           the framed, hashed audit export
│   ├── audit/            the independent rebuild
│   ├── wire/             protobuf conversions
│   └── pgtest/           a migrated template database per test binary
└── migrations/           goose SQL: schema, domains, routines, roles, trigger
```

Go with connect-go, pgx v5, goose, testcontainers-go and rapid. Writes go through PL/pgSQL routines and reads are a handful of queries, so hand-written pgx is smaller than generated code. Other repositories import only the `api` module. `buf breaking` checks every pull request against the last released API tag.

## 12. Production path (designed, not built)

The single-writer ledger is enough for the showcase. A separate production-scale design records what a production deployment adds and why each is deferred:

| Production concern | Where it would go |
| --- | --- |
| Regional durability: an asynchronous cross-region copy, an exposure monitor that pauses new commands above a lag or value threshold, and a self-fencing write lease | A durability decision record before any engine-specific build |
| Scale beyond one writer | Cells (one writer each) behind a routing layer, started when measured peak reaches half of the measured usable capacity |
| General-ledger feed | `accounting_date`, period close, a daily GL extract and tie-out evidence |
| Hot accounts | Sharded balance rows for house accounts, after measurement |
| Retention | Hot retention covering the longest reversal window, then a verified archive |
| Governance | Approvals for forced postings, corrections and control changes |

None of these changes the API in section 4.

## 13. Build tasks

L0 to L7 in [`plan.md`](plan.md). The ledger is done when it passes its own tests and Corridor's integrated scenarios without any other service writing to its database.
