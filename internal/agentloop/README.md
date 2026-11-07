# agentloop

Keel's **agent-loop service contract**: the replaceable strategy that drives one
run of a session. The default implementation is **`pi-loop`** — a thin adapter
over `internal/loop`, the ported PI loop — and any other loop may take the slot,
but only after it passes the PI conformance suite (§5).

The loop is the service that a run coordinator (the kernel side that is being
split out of `agent.Runner`) calls after it has rebuilt the transcript from the
committed event log:

```go
in := agentloop.Input{
	SessionID: run.SessionID.String(),
	Context: agentloop.Context{
		SystemPrompt: host.SystemPrompt(),
		Messages:     msgs,        // reconstructed from committed events
		PromptCount:  promptCount, // trailing user messages = this run's prompt
		Tools:        registry.List(),
	},
	Model:  agentloop.ModelConfig{ID: model, Provider: providerName, APIKey: apiKey, ThinkingLevel: agentcore.ThinkingLow},
	Limits: agentloop.Limits{MaxTurns: 8, EventBuffer: 32},
	Batch:  agenttool.BatchConfig{ToolExecutorConfig: agenttool.ToolExecutorConfig{Registry: registry}},
}
stream, err := loopService.Run(ctx, in)
for ev := range stream.Events() {
	switch e := ev.(type) {
	case agentcore.MessageEndEvent:       // assistant message → kernel.Commit(assistant.message, generationId)
	case agentcore.ToolExecutionEndEvent: // tool result        → kernel.Commit(tool.result, toolCallId)
	}
}
produced, err := stream.Result(ctx) // what this run produced, never the transcript
```

## 1. The interface

One method, one input state, one output stream:

| | |
|---|---|
| `Loop.Run(ctx, Input) (*RunStream, error)` | start one run over a complete input state |

`Input` carries everything the run needs and **nothing it must not have**:

| Field | Meaning |
|---|---|
| `SessionID` | run identity; carried in the first event (`agent_start`) |
| `Context.SystemPrompt` | host-assembled system prompt |
| `Context.Messages` | transcript reconstructed from committed events; the trailing `PromptCount` entries are this run's prompt |
| `Context.PromptCount` | how many trailing messages are the new prompt (0 = continuation run) |
| `Context.Tools` | model-facing tool set; the loop derives the executable registry from it when `Batch.Registry` is nil |
| `Context.TransformContext` / `ConvertToLlm` | PI's two provider-boundary conversion seams |
| `Model.*` | model id, provider, static API key, dynamic key resolver, thinking level, provider extras |
| `Limits.MaxTurns` | completed-turn cap; 0 selects `DefaultMaxTurns` (8, `agent.NewRunner`'s cap), negative = uncapped |
| `Limits.EventBuffer` | event channel buffer; 0 = synchronous back-pressure (PI's awaited emit), the default loadout uses 32 |
| `Compaction.*` | context window + compaction settings (disabled while window ≤ 0 or `Enabled` is false) |
| `Batch` | optional tool-execution wiring (prepare/before/after hooks, budgets); `Registry` nil → derived from `Tools` |
| `Hooks.*` | optional per-run seams (steering/follow-up queues, prepare, finish, stop, settle, compaction report) |

`Run` returns before the run starts. Errors are configuration problems only —
`ErrNoStream` (no `ctx.llm` binding), `ErrNothingToAnswer` (empty transcript,
system-only, or an assistant tail: append the next prompt first),
`ErrInvalidInput` (malformed `PromptCount`, duplicate tool names). Model and
tool failures are never errors: they arrive as a terminal assistant message with
`stopReason` `error`/`aborted` and as error tool results, exactly like PI.

`RunStream` is the producer/consumer pair of `agentcore.EventStream`, shaped for
the service boundary (a loop drives `Emit`/`Finish`, the coordinator consumes
`Events`/`Result` and may `Cancel`). `Result` is stable across reads and never
shares memory with the caller's input slice.

## 2. Event semantics

The stream carries `agentcore.AgentEvent` values in PI's order:

```
agent_start
  [message_start/message_end for the prompt tail and the tool-loadout declaration]
  turn_start, message_start → message_update* → message_end, tool_execution_start/end,
  message_start/message_end per tool result, turn_end
  … (one turn per provider response that calls tools)
agent_end { produced messages }
```

plus Keel's **additive observability events** (`telemetry`, `compaction`,
`compaction_start`, `subagent_progress`) which consumers dispatch by type and
may ignore. The stream result is the run's produced messages — the prompt it
echoed, the assistant messages, the tool results — and never the pre-existing
transcript.

Invariants the coordinator relies on (asserted for every implementation in
`contract_test.go`):

- exactly one `agent_end`, and it is the last event; its `Messages` equal the
  stream result;
- every finalized tool call has a matching tool-result message, in order, and no
  tool result dangles;
- every started `tool_execution_start` has its `tool_execution_end`;
- the result repeats no pre-existing transcript message.

## 3. Cancellation

Cancelling the run context — or calling `RunStream.Cancel` — ends the run: the
producer settles, sets the result and closes the stream, so a consumer always
gets an outcome. Emits that race the cancellation are best-effort (the
underlying stream selects on the run context), therefore: everything up to the
in-flight provider call / tool execution is deterministic, the tail after a
cancel is not. A run cancelled mid-stream still produces an `aborted` terminal
assistant message when the provider call was already in flight; on resume the
coordinator's `fillMissingToolResults` path (today's `agent.Runner`) owns
at-least-once completion of unresolved calls.

A consumer that stops reading events **before the run settles must `Cancel`**
(or cancel the context it passed to `Run`): the producer blocks on its next emit
until either happens — abandoning a stream without cancelling strands it.

## 4. Default `pi-loop`

`PiLoop` adds no behavior beyond the `Input` mapping. It delegates to
`loop.StartRun`, so event order, tool pairing and cancellation are whatever
`internal/loop` produces (the conformance suite pins that). What it does add:

- the turn cap (`Limits.MaxTurns` → a `ShouldStopAfterTurn` wrapper, at the same
  point `RunConfig` consults it; the cap short-circuits the hook);
- tool registry derivation (`Context.Tools` → registry) and declaration
  (`Context.Tools`, or the registry's `List()` when `Tools` is empty);
- a transcript copy, so a run never mutates the coordinator's slice.

`PiLoop` implements the `servicegraph.Instance` lifecycle with no-op `Start`/
`Stop`: it holds no runtime effects (each run's stream is created by `Run` and
released when it settles), it only exists so the graph can manage it uniformly.

Interface decisions worth knowing:

- **Hooks reuse `internal/loop`'s seam types** (`TurnUpdate`,
  `AgentTurnContext`, `RequestUpdate`, …) instead of aliasing or duplicating
  them: PI's hook contract has one source of truth. If a second production loop
  appears, extracting them into a neutral package is the follow-up move.
- **No provider stream in `Input`**: the stream is the instance's binding
  (`NewPiLoop(stream)` / the `ctx.llm` binding), because replacing the LLM
  provider in a new loadout generation must rebind the loop.
- **The compaction summary request uses the loop's own stream** (`SummaryStream`
  / `SummaryModel` are not exposed): the defaults (`internal/loop` falls back to
  the primary stream and synthesizes the summary model) cover the default
  loadout.
- **No separate `Continue` entry point**: the coordinator always hands over a
  complete transcript; `PromptCount = 0` is the continuation case.

## 5. Replacement gate: conformance

`pi-loop` is the default *and* the contract. A replacement loop:

1. implements `agentloop.Loop` (and, to be installed by the graph, the
   `servicegraph.Instance` lifecycle);
2. passes the **exported PI conformance suite**
   (`internal/agentloop/agentlooptest`, a normal package so loop authors can
   import it):

   ```go
   func TestMyLoopConformance(t *testing.T) {
       agentlooptest.Suite(t, func(t *testing.T, stream provider.StreamFn) agentloop.Loop {
           return myloop.New(stream)
       })
   }
   ```

   The factory receives the suite's scripted provider stream because a loop
   binds its provider at construction (the `ctx.llm` binding of a loadout
   generation), exactly like `PiLoop`. The suite drives the service interface
   only; its 23 cases keep PI's upstream `it(...)` titles and cover event order,
   tool call ↔ result pairing (completion vs source order, truncation,
   terminate), cancellation, error paths, transcript equivalence and the
   steering/follow-up/prepare/transform seams. `internal/loop`'s suite remains
   the line-by-line PI diff for the default implementation;
3. declares the manifest it passed. `agentloop.Manifest()` is the canonical,
   sorted case list and `agentloop.ManifestHash()` its SHA-256
   (`agentlooptest` re-exports both). The default blueprint pins pi-loop's hash
   in `loadout.PinnedPiLoopManifestHash` (a test fails on suite drift), and
   `loadout.ValidateLoopConformance` rejects any `ctx.loop` service in a
   loadout that has no declaration, no pin, or a manifest hash that does not
   match the pin. The gate is **fail-closed**: omission is rejection, never a
   silent pass;
4. then, and only then, may a loadout select it (`DescriptorWithFactory`, or a
   `testloop`-style implementation in tests).

`testloop` (this package's subpackage) is the deliberate counter-example: a
minimal, deterministic canned loop that satisfies the interface and the
invariants, but **never calls the model and therefore cannot pass PI
conformance**. It exercises the replacement path in tests; it must never appear
in a production loadout. A loadout that selects it is a bug. The suite's own
tests prove the gate rejects both the canned loop and a deliberately broken
loop through case assertions (not compile errors).

## 6. Boundary with the kernel and the graph

- **The loop does not own the ledger.** `Loop`, `Input`, `RunStream`, `Hooks`
  and both implementations reference no ledger package: no store, no kernel, no
  lease, no session handle. Everything the loop produces is a proposal (events,
  messages); `kernel.Commit` validates kinds, idempotency keys, pairing and
  fencing, and the coordinator owns the run cursor and lifecycle.
- `Hooks.OnCompaction` **reports** a summary; persisting it as a checkpoint is
  the coordinator's call. A hook must not append events either.
- **Against the graph**: the descriptor (`Descriptor()`) declares `pi-loop` —
  provides `ctx.loop`, requires `ctx.llm` + `ctx.tools`, optional
  `ctx.compaction`, effect `loop-stream` — with a factory that rebinds the llm
  provider on every assembly (`BindStream`). Service names and capabilities
  mirror `internal/loadout`'s blueprint and a test fails on drift. This package
  depends on `servicegraph` types only (descriptor/lifecycle), never on the
  engine; the engine remains W2's integration concern.

The boundary is enforced structurally, not by convention:

- `TestLoopSurfaceHasNoLedgerTypes` walks the exported surface (including
  function signatures and struct fields) and fails on any type from
  `internal/{store,kernel,durable,httpapi,agent,host}`;
- `TestLoopImplementationImportsStayInsideTheServiceBoundary` parses the
  implementation sources and fails on such an import.

## 7. Test map

| File | Covers |
|---|---|
| `contract_test.go` | shared invariants run against `pi-loop` (text, tools) and against `testloop`, `LoopFunc`, transcript-not-result |
| `pi_test.go` | PI event sequence for text/tools, tool pairing, cancellation, turn cap, input validation, provided registry, no input mutation, result stability |
| `equivalence_test.go` | adapter vs `internal/loop` driven with `agent.Runner`'s config: event types, transcript, provider requests, and cancellation prefix equivalence |
| `conformance_suite_test.go` | the exported PI conformance suite run against `pi-loop`; manifest hash equals the loadout pin |
| `manifest.go` | the PI conformance `Case` list, canonical JSON hash, and the loadout pin's source of truth |
| `agentlooptest/` | the exported suite (`Suite`, `Manifest`, `ManifestHash`), manifest↔case lockstep/hash tests, and the broken/canned-loop rejection proofs |
| `descriptor_test.go` | descriptor vs `loadout` blueprint and vocabulary, factory binding (bare fn, `StreamProvider`, failures), static graph resolution, slot replacement with `testloop` |
| `boundary_test.go` | no-ledger type and import guards |
| `testloop/` | the canned alternate loop used by the tests above and by W2/W3 loadout tests |

All tests are deterministic (scripted provider, script-supplied tool call ids,
no clock in fixtures) and run under `-race`.
