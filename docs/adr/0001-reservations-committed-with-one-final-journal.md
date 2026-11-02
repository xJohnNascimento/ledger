# ADR 0001: Ledger reservations, committed or released with one final journal

Status: accepted (2026-11-02)

**Context.** Funds must be checked before the provider trades or pays. The real outcome, such as the amount actually delivered, is known only later, and a failed payout must return the money. Balances must never be wrong in between.

**Decision.** The ledger offers reservations: maximum reductions on several enforced accounts, reserved atomically with an immutable settlement scope. A commit posts the final balanced journal (the conversion and any fill difference in one entry); a release can post a return journal in the same transaction. Exactly one final transition wins.

**Consequences.** Provider cash moves by exactly what the provider moved, atomically. There is no window between voiding a hold and returning the money, so Corridor has fewer recovery cases. The ledger API is richer than plain two-phase transfers and needs its own concurrency tests.

**Alternatives considered.** Pending entries posted or voided, plus separate journals for differences and returns (two more crash windows; two-phase transfers in the TigerBeetle style cannot add legs at posting time); posting immediately and compensating later (money moves before the outcome is known).
