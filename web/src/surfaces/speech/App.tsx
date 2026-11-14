// The speech harness's app entry. `nsh test` resolves the app around a test
// file to the nearest directory with an `nsh.json`; this directory carries one
// so the workbench harnesses render with the product's own theme
// (`web/theme.json`) and the `speech` capability, and needs an entry beside it.
// The screens are `harness.tsx` / `harness-demo.tsx`; importing the plain one
// here is enough.
//
// Not part of the product build: `web/App.tsx` is the app.
import "./harness";
