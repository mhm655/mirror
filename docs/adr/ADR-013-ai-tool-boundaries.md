# ADR-013 — AI tool permission boundaries

**Status:** accepted · **Date:** 2026-08-30

## Context

The platform includes an AI operations assistant with access to the running
simulation. The brief was explicit: "AI agents must NOT automatically receive
unrestricted production-like capabilities. Dangerous actions should require
explicit authorization." This ADR records how that requirement is actually
enforced in code, not just in a system prompt.

## Decision

Every tool the agent can call is a thin wrapper over the same
`simctl`/`engine` APIs the HTTP layer uses — there is no privileged internal
path. Tools are partitioned into three tiers:

- **read** — always available; cannot change anything.
- **sandboxed** — forks the simulation and operates only on the fork.
- **mutate** — changes the live simulation; gated by the *intersection* of a
  per-request grant (`approveMutations` in the chat request) and the caller's
  own role (must be at least operator).

Additionally, the set of injectable event kinds the agent may use
(`agentInjectable`) deliberately excludes earthquakes and every chaos-lab
command, even under a mutation grant.

## Reasoning

"No privileged path" is the load-bearing part. If the agent's tools called into
some separate, more-trusted internal API than the one an operator's own browser
session uses, then a prompt-injection or reasoning failure could make the agent
do something an operator physically could not do through the UI. Routing every
tool through the exact same `simctl.Manager` methods the REST handlers call
means the blast radius of the AI layer is provably bounded by the blast radius
of the API layer, which is already reviewed as the trust boundary.

The **sandboxed tier being unrestricted by default** is a deliberate asymmetry,
not an oversight. Forking is safe to hand to an AI with no additional gate
because a fork cannot, by construction (ADR-004's partition-independence and
the state-clone semantics), affect its parent. Counterfactual reasoning —
"fork three ways, run them, compare" — is exactly the capability an operations
agent exists to provide, and it is also the one capability that is safe to
grant unconditionally. Gating it behind the same approval as a live mutation
would make the agent nearly useless for its primary purpose while adding no
actual safety.

**Mutation authority as an intersection, not a union**, closes a specific
privilege-escalation shape: a viewer-role user cannot get the agent to mutate a
live simulation just by ticking "allow mutations" in their own chat request,
because the server-side check is `req.Approve && key.Role >= RoleOperator`. The
UI checkbox alone does not extend a caller's own authority through the agent as
a side channel.

**Excluding earthquakes and chaos commands from the agent's allow-list**,
separately from the tier system, encodes a judgment call: some actions are
appropriate for a human operator to take deliberately (a routine accident
injection, a policy change) but are not appropriate to leave to an AI's
judgment about when they're warranted, regardless of role. This is a
conservative, hand-maintained list rather than a learned or configurable
policy, on purpose — it should require a deliberate code change to expand.

## Consequences

- Every tool call is surfaced in the chat transcript (`Step` records: tool
  name, tier, input, output, timing, error) rather than only the model's final
  prose response. An agent that reported a conclusion without showing which
  tools produced it would be indistinguishable from one that fabricated the
  conclusion, so this is treated as a hard requirement, not a debugging nicety.
- The deterministic built-in planner (used when no LLM API key is configured,
  or as an LLM-outage fallback) is held to the identical tool boundary — it
  cannot call anything the LLM path could not also call for the same request.
  Removing the language model does not create a side door.
- Extending the mutate tier (e.g., adding a new injectable event kind) is a
  one-line change to an explicit allow-list map, which keeps the "what can the
  AI actually do" question answerable by reading one place in the code.
