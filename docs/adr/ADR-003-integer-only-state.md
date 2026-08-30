# ADR-003 — Integer-only simulation state

**Status:** accepted · **Date:** 2026-08-30

## Context

Physical simulation naturally wants real numbers: positions, speeds, congestion
factors, probabilities.

## Decision

No floating point in simulation state, ever. Millimetres, mm/tick, and permille
factors, all integers. Floats appear only at the presentation boundary — JSON
metrics, the wire protocol, the renderer.

## Reasoning

IEEE-754 arithmetic is deterministic *in principle*: the standard specifies the
result of every basic operation exactly. In practice three things break it.

1. **FMA contraction.** A compiler may fuse `a*b + c` into a single fused
   multiply-add with one rounding instead of two. Whether it does depends on the
   target architecture and the compiler version. Results differ in the last bit,
   and in a simulation with feedback that difference compounds into a completely
   different afternoon.
2. **Transcendental functions.** `sin`, `exp` and friends are not specified to
   the last bit by IEEE-754. Different libm implementations differ.
3. **x87 excess precision.** Mostly historical, still real on some targets.

A digital twin whose entire value proposition is "same inputs, same outputs"
cannot rest on "should be fine as long as nobody changes architecture".

Integers have none of these problems. The cost is that division must be handled
explicitly, which turns out to be a benefit: `MulP` and `DivRound` round
half-away-from-zero, so a result does not depend on the sign of an intermediate
value — whereas Go's native truncation toward zero does, and that asymmetry is a
classic replay-divergence source.

## The one place hardware floating point is used

`units.ISqrt` seeds from `math.Sqrt` and then corrects with two integer loops
that force the exact integer root. This is not a violation: the *output* is an
exact integer that depends on nothing but the input. IEEE-754 `sqrt` is
correctly rounded and therefore identical on every conforming platform, and the
correction removes any dependence on which way that rounding went. See ADR-004.

## Consequences

- Every scaling factor is permille. Verbose, and unambiguous.
- `TestNoFloatInState` walks `state.State` by reflection and fails on any float
  field, so this decision cannot be quietly eroded by a later change.
- Rendering, metrics and the JSON API convert to float at the boundary, where
  divergence is harmless and where a human is going to round it anyway.
