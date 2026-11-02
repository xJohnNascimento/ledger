# ADR 0002: A single-writer PostgreSQL ledger

Status: accepted (2026-11-02)

**Context.** The ledger must be correct under concurrency and easy to explain, and the showcase runs on a laptop. A production-scale design for regional durability and scale-out was worked out separately; this repository builds its single-writer core.

**Decision.** One PostgreSQL writer. Mutations run in reviewed PL/pgSQL routines, called by a role that cannot write tables directly; `READ COMMITTED` with an explicit lock order; a deferred trigger rechecks every entry; amounts are integral `numeric` within signed 128 bits. Cross-region durability, cells, the GL extract, archiving and governed approvals stay designed, not built.

**Consequences.** Locks are held only for in-database work. The database refuses unauthorized edits. The throughput ceiling is measured by `ledger bench` and explained, not hidden.

**Alternatives considered.** Ledger logic in Go holding row locks across round trips (longer lock times); a specialized ledger database (less to show); building the production-scale design (needs infrastructure a local build cannot exercise).
