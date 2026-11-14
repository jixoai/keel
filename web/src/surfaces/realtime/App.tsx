// The realtime harness's app entry. `nsh test` resolves the app around a test
// file to the nearest directory with an `nsh.json`; this directory carries one
// so the surface's host calls (`keel.realtime.*`) are granted, and needs an
// entry beside it. The screen itself is `harness.tsx`, which mounts
// `RealtimeSurface`; importing it here is enough.
//
// Not part of the product build: `web/App.tsx` is the app.
import "./harness";
