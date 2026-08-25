# ADR-011: Every numeric resource limit is an implementation default, never a spec mandate

## Status
Accepted

## Context
The OPC XML-DA 1.0 specification defines no numeric limits anywhere: no maximum item count per request or
subscription, no maximum concurrent subscriptions, no maximum message/request size, no rate limiting
(REQ-LIMITS-001, `open-questions.md` OQ-3). The only guidance is a loose remark that "the server specific
maximum time will generally be no more than a minute or two" (§3.1.6, p.35). A server library must still
have *some* limits to avoid unbounded resource consumption from untrusted client input (the project
directive explicitly requires this).

## Decision
Every such limit is a field on `server.Config` with a conservative, explicitly-documented default:
`MaxItemsPerRequest`, `MaxItemsPerSubscription`, `MaxConcurrentSubscriptions`, `MaxRequestBodyBytes`,
`RequestTimeout`, `MaxPolledRefreshWait`, `MaxConcurrentPolls`, `ReapInterval`, `ReapGraceMultiplier`,
`DefaultSubscriptionPingRate` (substituted when a client sends `SubscriptionPingRate=0` — see
`docs/specification/open-questions.md` OQ-10; this one is a default *substitution value* rather than a
*limit*, but is documented alongside the limits since it is likewise an implementation policy the spec does
not mandate a number for). Each
one's GoDoc and the config documentation explicitly state "implementation default, not spec-mandated" so
neither a library maintainer nor a downstream operator ever mistakes a chosen number for a protocol
requirement.

## Alternatives considered
- **No limits at all** (rely entirely on downstream applications to impose them): rejected — conflicts
  directly with the project's security/robustness requirements (unbounded body/item-count/concurrent-
  subscription growth from untrusted input must not be possible by default).
- **Hard-coded, non-configurable limits**: rejected — different deployments have legitimately different
  scale needs; every limit is a `Config` field precisely so it can be tuned without a code change.

## Consequences
- `docs/limitations.md` and `docs/server-configuration.md` must list every default value and its rationale,
  and must never describe them as spec requirements.
- Adding a spec-mandated limit in some hypothetical future OPC XML-DA revision would be a straightforward,
  additive change to `Config` — the mechanism (a documented, defaulted config field) is already in place.
