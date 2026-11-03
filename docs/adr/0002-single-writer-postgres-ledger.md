# ADR 0002: A single-writer PostgreSQL ledger

Status: accepted (2026-11-02)

**Context.** The ledger must be correct under concurrency and easy to explain, and the showcase runs on a laptop. Regional durability and scale-out are real production concerns, but they need infrastructure a local build cannot exercise; this repository builds the single-writer core and lists the rest in [design, section 12](../design.md#12-production-path-designed-not-built).

**Decision.** One PostgreSQL writer. Mutations run in reviewed PL/pgSQL routines, called by a role that cannot write tables directly; `READ COMMITTED` with an explicit lock order; a deferred trigger rechecks every entry; amounts are integral `numeric` within signed 128 bits. Cross-region durability, cells, the GL extract, archiving and governed approvals stay designed, not built.

**Consequences.** Locks are held only for in-database work. The database refuses unauthorized edits. The throughput ceiling is measured by `ledger bench` and explained, not hidden.

**Alternatives considered.** Ledger logic in Go holding row locks across round trips (longer lock times); a specialized ledger database (less to show); building the production path of design section 12 now (needs infrastructure a local build cannot exercise).
