# speech/decision runtime + media artifact event evidence (2026-11-21, lane S2)

Worktree: .worktrees/keel-speech-decision (main 4a68729)
Code commits: 5d0aae3 feat(media): wire speech and decision runtime tools;
              54aabd5 fix(media): reject empty decision outcomes;
              2ea0f42 fix(media): reject multipart header injection in STT
DSN: read from .env.local as KEEL_TEST_DSN; the value is never printed.

## What is wired

- speech_tts / speech_stt: internal/mediaadapter/speech_tools.go:38,72,139,173
  * tenant tts/stt provider -> internal/mediawiring/speech.go:32
  * TTS audio -> layerfs; result carries blobId/mime/size/duration + chars/bytes/ms
  * STT reads a layerfs sha256 blob_id, posts multipart, returns transcript/segments
    + the input-audio provenance reference
  * missing credential -> typed missing_credentials, zero wire calls
- decision_ask: internal/mediaadapter/decision_tools.go:59,99
  * tenant decision provider + saved binding -> internal/mediawiring/decision.go:108
  * policy (mode/failPolicy/threshold) only from the binding; model arguments
    cannot switch enforce->shadow or fail-open
  * enforce failure -> typed provider_unavailable with "decision unavailable
    (fail-closed)", no answers; shadow failure -> shadow=true + failure + no answers
  * no noul:0 / choice:"unknown" / confidence:0 synthesized
- media artifact event: reuse tool.result.details.media
  * helper: internal/mediaadapter/tools.go:121
  * image_generate/video_generate also carry the same details (existing tools)
  * no new kernel EventKind; layerfs sha256 refs are retained by layerfs, while
    kernel blob_refs counts platform UUID blobs
- crash isolation: internal/mediaadapter/tools.go:103
  * adapter guard already covers provider panics; this tool-level recovery also
    covers adapter/seam panics (Store, Executor), returning retryable
    provider_unavailable without poisoning sibling tools

## STT multipart header boundary (A-axis P2 fix)

- internal/mediaadapter/speech.go: normalizeTranscribeMetadata
  * both SpeechAdapter.Transcribe and HTTPSpeechProvider.Transcribe normalize
    the model-controlled filename/mime before any wire call
  * filename/mime containing CR, LF or any Unicode control character is
    rejected with typed invalid_request (field-level filename/mime error);
    it is never trimmed into a different safe value
  * mime is lowercased and must be exactly one bare type/subtype using RFC 7230
    token characters; parameters, whitespace and separators are refused
  * quotes/backslashes remain legal and are escaped by escapeQuotes; the
    regression test parses the wire part and proves one Content-Disposition,
    the expected filename and no injected X-Evil header
  * empty metadata still defaults to filename "audio" and
    application/octet-stream
- tests: internal/mediaadapter/speech_test.go (CRLF/LF/NUL/tab, mime CRLF/
  control/parameters/whitespace, quoted filename, defaults) and
  internal/mediaadapter/media_tools_test.go (model tool arg path rejects before
  any HTTP request)

## Gates

See gates.txt:
- gofmt -l internal/mediaadapter internal/mediawiring internal/servicehost cmd/keel: clean
- go build ./...: exit 0
- go vet ./...: exit 0
- go test -p 1 -count=1 ./... (KEEL_TEST_DSN from .env.local): exit 0
  (full-go-test.txt)
- go test -race -count=1 ./internal/mediaadapter ./internal/mediawiring
  ./internal/servicehost: exit 0 (race.txt)
- scripts/conformance.sh: TOTAL 1097 pass / 0 fail / 1 skip, exit 0
  (conformance.txt, conformance.json). The first parallel run hit one
  pre-existing shared-DB flake in internal/agent
  (TestCompactionPersistsEventAndCheckpoint); the isolated rerun and the second
  full conformance run both passed.
- openspec validate keel-agent-team-v1 --strict --no-interactive: valid
  (openspec-strict.txt)

## Focused evidence

- adapter-tools.txt: registry + speech/decision tool tests, panic isolation,
  details/usage checks
- wiring.txt: tenant-config assembly, TTS/STT layerfs round trip, decision
  saved binding, missing-credential fail-closed
- run-e2e.txt: real worker run with a scripted chat model and recording HTTP
  fixture: TTS, STT, decision success, enforce failure, shadow failure;
  image_generate details regression

## Not done / boundaries

- No real vendor was called; all wires are fake/recording providers.
- Cross-process provider-plugin isolation remains the provider plugin layer's
  job (unchanged by this lane); only the media tool boundary is covered here.
- The human-facing /v1/media/audio demo routes remain internal/httpapi/media_audio.go;
  they are not the tenant runtime.
- STT input is a layerfs reference (normally speech_tts output or a file tool),
  not a user upload UUID.
