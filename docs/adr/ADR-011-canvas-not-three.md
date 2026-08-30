# ADR-011 — Canvas2D, not Three.js

**Status:** accepted · **Date:** 2026-08-30

## Context

The brief called for Three.js "only where it adds value" and warned explicitly
against an unnecessarily flashy 3D visualisation. The map view needs to show
tens of thousands of point entities and up to ~40,000 road links, pannable and
zoomable across an 8 km city, while staying legible to an operator making a
decision.

## Decision

Canvas2D (`web/src/render/renderer.ts`), not Three.js / WebGL.

## Reasoning

The job here is a 2D information display, and a perspective 3D camera actively
works against it. A perspective camera puts the far side of the city at a
different apparent scale than the near side — exactly wrong when the point of
the view is to compare congestion across districts at a glance. Extruded
buildings would add occlusion that hides the thing the page exists to show:
traffic flow and infrastructure state, not skyline.

WebGL's actual advantage — raw fill rate at very high point-sprite counts —
does not apply at the scale this system streams to a browser. The server
viewport-culls and caps the vehicle stream (see the blueprint's transport
section) specifically because a screen a few hundred pixels wide cannot resolve
more than a few thousand distinguishable dots regardless of renderer; sending
more would cost bandwidth and decode time for information the eye discards.
Within that bound, Canvas2D is comfortably inside frame budget, measured at 60
fps with interpolated vehicle motion between the 8 Hz server frames.

The road network is drawn on an offscreen canvas, rebuilt only when the camera
moves or a new 2 Hz congestion frame arrives; vehicles are redrawn every
animation frame on top. That two-layer split, not the choice of 2D vs 3D API, is
what actually keeps the frame rate smooth.

## Consequences

- No 3D camera controller, no shader pipeline, no picking-via-raycasting to
  build or maintain. Hit testing is a straightforward nearest-point/segment
  search over the same coordinate space used for drawing.
- If a future requirement genuinely needs 3D — for example, an underground
  transit layer that must show grade separation — that is a real trigger to
  revisit this decision. Nothing about the current data model or wire protocol
  would need to change to support it; only the renderer would.
- This is explicitly called out on the "what not to build" list, because a 3D
  city view is the single most likely thing a reviewer expects to see and does
  not get, and the reason it is absent is a design decision, not an omission.
