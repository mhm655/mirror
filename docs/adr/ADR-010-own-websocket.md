# ADR-010 — Own WebSocket implementation

**Status:** accepted · **Date:** 2026-08-30

## Context

The live stream needs an RFC 6455 WebSocket server endpoint. Mature Go libraries
exist (`gorilla/websocket`, `nhooyr.io/websocket`).

## Decision

A minimal implementation in `internal/api/websocket.go`, covering the opening
handshake, binary/text/ping/pong/close frames, client-frame unmasking,
fragmentation, and the control-frame size limit — and explicitly not covering
compression extensions, subprotocol negotiation beyond echoing one back, or the
client role.

## Reasoning

This is a narrow exception to "don't reinvent the wheel," and the narrowness is
the point: the server sends exactly one kind of message on the hot path — a
binary frame to potentially many clients, up to 8 times a second — and needs
precise control over buffer reuse on that path. A vehicle frame is rebuilt and
broadcast to every connected client many times a second; allocating a fresh
frame buffer per client per tick would hand the garbage collector real,
continuous pressure on exactly the code path that most needs to stay
allocation-free. A general-purpose library is not wrong to carry a
permessage-deflate negotiator, an extension registry, and a client dialer — but
this process will never use any of them, and "vendor a general library, then
structure all the hot-path code around its buffer-ownership model" is more
integration work than writing the ~40 lines of frame parsing this actually
needs.

The scope is also small enough to fully specify: one role (server), no
extensions, and RFC 6455's masking requirement is enforced explicitly (a server
MUST close the connection on an unmasked client frame) because skipping it is a
real, if unglamorous, proxy cache-poisoning vector.

## Consequences

- No compression. Payloads are already compact by design (see the blueprint's
  wire-protocol section) so this is not currently a cost.
- No client role, so this cannot be reused for outbound WebSocket connections
  elsewhere in the codebase without extension.
- Anything unsupported is rejected explicitly (a close frame with a specific
  code) rather than silently mishandled, per the file's own stated design rule.
- This is the correct call at this scope and would be the wrong call if the
  server needed to speak WebSocket to arbitrary external clients with unknown
  extension needs — at that point, adopt a library.
