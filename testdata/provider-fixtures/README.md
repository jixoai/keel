# Provider fixtures (recorded from the real gateway)

These fixtures were captured from the real configured provider
(`KEEL_BASE_URL`, model-prefix `bailian-token-plan`) by
`scripts/record-provider-fixtures.mjs`. They are meant to be replayed by mock
tests; no network is needed to consume them.

## Redaction policy

Nothing in this directory contains the API key or an Authorization header.
Also removed/replaced:

- request ids (`request_id`, `chatcmpl-*` ids) → `<redacted-id>` / `<redacted-request-id>`
- provider timestamps (`created`, `created_at`, asset expiry) → `0` / `<redacted-timestamp>`
- signed asset URLs → `https://<redacted-asset-host>/...` (query strings are dropped entirely)

## Replay

All calls use bearer auth sourced from `KEEL_API_KEY` against `KEEL_BASE_URL`
(no `/v1` prefix in this deployment). Do not commit the key.

| Fixture | Request | Expected status |
| --- | --- | --- |
| `llm/chat-qwen38-flash.json` | `POST /chat/completions` `{"model":"bailian-token-plan/qwen3.8-flash","messages":[{"role":"user","content":"用一句话介绍你自己"}],"max_tokens":64}` | 200 |
| `llm/chat-qwen38-flash.stream.txt` | same body plus `"stream":true`; response is SSE (`data: {...}` lines, terminated by `data: [DONE]`) | 200 |
| `image/generate-wan27.json` | `POST /images/generations` `{"model":"bailian-token-plan/wan2.7-image","prompt":"极简玻璃信鸽，深蓝背景，产品渲染","size":"1024x1024","n":1}` | 200 |
| `image/edit-wan27.json` | `POST /images/edits` `{"model":"bailian-token-plan/wan2.7-image","prompt":"把背景改成暖白色，保留信鸽","image":"<provider URL from generate>"}` | 200 |
| `image/edit-dataurl-error.json` | `POST /images/edits` with a 1x1 PNG `data:` URL | **400** (resolution, not scheme; see below) |
| `decision/choice.json` | `POST /decisions` `{"model":"bailian-token-plan/decision-model-preview","input":"Pick A or B, answer one letter.","questions":[{"type":"choice","predicate":"the better option","choices":[{"value":"A"},{"value":"B"}]}]}` | 200 |
| `decision/predicate.json` | `POST /decisions` `input:"The server is down and nobody can log in."`, `questions:[{"type":"predicate","predicate":"this is a production incident"}]` | 200 |
| `decision/predicate-transient-502.json` | `POST /decisions` `input:"Is the message urgent? Answer yes or no."`, `predicate:"the message is urgent"` | **502** (upstream connection reset; retried identically with the same result) |
| `decision/score-error.json` | requested score shape with `criteria:[{value,description}]` | **400** (recorded deliberately) |
| `decision/score-hint.json` | `levels:["bad","great"]` (strings) | **400**; the message reveals the required struct |
| `decision/score.json` | `levels:[{"label":"bad","description":"bad"},{"label":"great","description":"great"}]` | 200 |
| `models/catalog.json` | `GET /models` projected to `id`/`type`/`kind`/`modalities`/`native_endpoints` (no pricing) | 200 |
| `capabilities/*.json` | see below | 401/404/400 |

### Decision score schema

The requested `criteria` field is **not** the provider's schema. The provider
returns:

```
HTTP 400: questions.question_1.criteria: 0 score levels outside the allowed 1..255
```

The accepted shape is `levels`, an array of objects with `label` and
`description` (string levels are rejected with
`cannot unmarshal string into Go struct field question.levels of type struct { Label string; Description string }`).
Response probabilities echo the supplied labels and use zero-based `value`
indices.

### Image edit findings

`POST /images/edits` accepts the same `{model,prompt,image}` JSON as the
gateway's normalized edit request. A provider http(s) URL returned by
`/images/generations` worked (HTTP 200, same `data[].url` shape as generation).

A `data:` URL is **not rejected by scheme** in this deployment: the 1x1 PNG in
`image/edit-dataurl-error.json` was decoded/forwarded and the provider rejected
it on **resolution**, not on transport:

```
HTTP 400: token-plan.cn-beijing.maas.aliyuncs.com: 400 Bad Request:
Error validating image, image <asset URL> resolution must be at least 240x240, got 1x1 (InvalidParameter)
```

So keel's client-side rejection of data URLs is a policy choice (and protects
against SSRF/large bodies), not a provider limitation. Whether a >=240x240 data
URL succeeds was not probed to respect the two-call budget for this route.

## Capability findings for this token plan

- `/models` returned **154** entries.
- `kind` field: **absent from every entry**. Models with `kind=video`: **none**.
- Models with `modalities.output` containing video: **none**; audio/speech output: **none**.
- Video-named ids: `auto/wan3.0-video`, `bailian-token-plan/wan3.0-video` (their `native_endpoints` are chat/messages only, not a video API).
- Audio/TTS/STT/realtime/embedding-named ids: **none**.
- Chat models that accept `video` **input** (understanding, not generation): 34 in total (e.g. `group/cheap-first`, `auto/kimi-k2.7-code`, `auto/kimi-k3`).
- Chat models that accept `audio` **input** (not speech synthesis): `auto/mimo-v2.6-flash`, `auto/mimo-v2.6-pro`, `auto/gemini-4-argon`, `opencode-gaubee/muse-spark-1.2-contributor`, `opencode-gaubee/muse-spark-1.3-contributor`, `opencode-gaubee/mimo-v2.6-flash`, `opencode-gaubee/mimo-v2.6-pro`, `opencode-gaubee/gemini-4-argon`, `opendesign/mimo-v2.6-flash`, `opendesign/mimo-v2.6-pro`.

### Probe results (exact errors, sanitized)

- `capabilities/video-generate.json` — HTTP 401: `open the link magpie web printed when it started: it carries the key. This browser sent no sign-in cookie for this address. A browser keeps cookies per host, not per port, so another service on this host's other ports can take it away: by clearing its site data (a sign-out often does), or by filling the host's cookie limit. A page opened at another address than the one signed in at (an IP instead of a name) has none either. `

- `capabilities/video-chat.json` — HTTP 400: `Bailian Token Plan: url error, please check url！ For details, see: https://<redacted-asset-host>/...`

- `capabilities/tts.json` — HTTP 404: `token-plan.cn-beijing.maas.aliyuncs.com: 404 Not Found: {"code":"InvalidParameter","message":"Model not exist.","request_id":"<redacted-request-id>"} — Bailian Token Plan may not serve this API`

- `capabilities/tts-qwen.json` — HTTP 404, same body (`model:"bailian-token-plan/qwen-tts"`), so the route itself is not served rather than a single wrong model id.

- `capabilities/stt-json.json` — HTTP 400: `send the audio as multipart/form-data, as OpenAI's /v1/audio/transcriptions takes it: a file and a model`

- `capabilities/stt-multipart.json` — HTTP 404: `token-plan.cn-beijing.maas.aliyuncs.com: 404 Not Found: {"code":"NotFound","message":"Api not found.","request_id":"<redacted-request-id>"} — Bailian Token Plan may not serve this API`. An earlier recording of the same probe returned HTTP 502 (`Post "<asset URL>": EOF`); both are upstream refusals, neither transcribes.

- `decision/predicate-transient-502.json` — one predicate request body returned HTTP 502 twice (`read: connection reset by peer`), while a different predicate body returned 200 (`decision/predicate.json`). Treat the 502 as request/route flakiness, not missing support.

- `capabilities/realtime.json` — HTTP 401: `open the link magpie web printed when it started: it carries the key. This browser sent no sign-in cookie for this address. A browser keeps cookies per host, not per port, so another service on this host's other ports can take it away: by clearing its site data (a sign-out often does), or by filling the host's cookie limit. A page opened at another address than the one signed in at (an IP instead of a name) has none either. `

Conclusion: **video generation, TTS, STT and realtime are not served by this
token plan**. The gateway's `/models` list advertises no such models, and the
dedicated routes either 404 upstream ("Bailian Token Plan may not serve this
API") or 401 against the gateway's browser-cookie fallback. The only
video-named id, `bailian-token-plan/wan3.0-video`, declares chat/messages
native endpoints only; a text-only chat call to it returns HTTP 400
(`Bailian Token Plan: url error, please check url！`), so there is no
text-to-video generation path.

Image generation **is** served (`/images/generations`, HTTP 200) and image
edit **is** served (`/images/edits`, HTTP 200 with a provider URL reference),
even though `wan2.7-image` is not listed in `/models`.

## Regenerate

The image-edit fixtures (`image/edit-*.json`) and
`decision/predicate-transient-502.json` were captured by a one-off live probe
using the same scrubbers; `scripts/record-provider-fixtures.mjs` does not yet
probe `/images/edits`.

```sh
node scripts/record-provider-fixtures.mjs                # core fixtures
node scripts/record-provider-fixtures.mjs --capabilities # + capability probes
```

The script reads the main repo `.env.personal` by default; override with
`KEEL_ENV_FILE`. It never prints the key and refuses to write any fixture that
still contains it.
