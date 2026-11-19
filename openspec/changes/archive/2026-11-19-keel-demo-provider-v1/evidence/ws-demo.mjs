// Realtime surface E2E against the temp demo instance (mirrors what web/host.js
// sends: session.update on open, then response.create; records the provider's
// event sequence verbatim).
import WebSocket from '/Users/kzf/Development/GitHub/jixoai/fde2e/fde2e-bu-v1/node_modules/ws/index.js'

const [, , base, sessionId] = process.argv
const password = process.env.KEEL_SERVE_PASSWORD
if (!base || !sessionId || !password) {
  console.error('usage: node ws-demo.mjs <base> <sessionId> (KEEL_SERVE_PASSWORD in env)')
  process.exit(2)
}
const url = `${base.replace(/^http/, 'ws')}/v1/realtime?session_id=${sessionId}&model=demo-realtime&provider_id=demo`
const events = []
const t0 = Date.now()
const ws = new WebSocket(url, { headers: { Authorization: `Bearer ${password}` } })

const timer = setTimeout(() => { console.log('TIMEOUT'); finish(2) }, 15000)
let sentSession = false
let sentCreate = false

ws.on('open', () => {
  events.push({ ms: Date.now() - t0, dir: 'open' })
  ws.send(JSON.stringify({
    type: 'session.update',
    session: {
      modalities: ['text', 'audio'],
      voice: 'demo',
      input_audio_format: 'pcm16',
      output_audio_format: 'pcm16',
      input_audio_transcription: { model: 'demo-stt' },
      turn_detection: { type: 'server_vad', threshold: 0.5, prefix_padding_ms: 300, silence_duration_ms: 700, create_response: true, interrupt_response: true },
      audio: { input: { format: { type: 'pcm', sample_rate: 16000, sample_format: 's16le', channels: 1, packing: 'interleaved', channel_layout: 'mono' } }, output: { voice: 'demo', format: { type: 'pcm', sample_rate: 24000 } } },
    },
  }))
  sentSession = true
})

ws.on('message', (raw) => {
  let msg
  try { msg = JSON.parse(String(raw)) } catch { msg = { type: 'unparsed', raw: String(raw).slice(0, 120) } }
  events.push({ ms: Date.now() - t0, dir: 'recv', type: msg.type, delta: msg.delta, transcript: msg.transcript })
  if (msg.type === 'session.updated' && !sentCreate) {
    sentCreate = true
    setTimeout(() => ws.send(JSON.stringify({ type: 'response.create' })), 100)
  }
  if (msg.type === 'response.done') {
    setTimeout(() => finish(0), 150)
  }
})

ws.on('error', (err) => { events.push({ dir: 'error', message: String(err.message) }); finish(2) })
ws.on('close', (code, reason) => { events.push({ dir: 'close', code, reason: String(reason) }); finish(0) })

function finish(code) {
  clearTimeout(timer)
  const types = events.filter((e) => e.dir === 'recv').map((e) => e.type)
  console.log('url:', url)
  console.log('sent: session.update=' + sentSession + ' response.create=' + sentCreate)
  console.log('received sequence:')
  for (const e of events) {
    if (e.dir === 'recv') console.log(`  ${e.ms}ms ${e.type}${e.delta ? ' delta=' + JSON.stringify(e.delta) : ''}${e.transcript ? ' transcript=' + JSON.stringify(e.transcript) : ''}`)
    else console.log(`  ${e.ms}ms (${e.dir}) ${e.code !== undefined ? e.code + ' ' + e.reason : (e.message ?? '')}`)
  }
  const d = events.filter((e) => e.dir === 'recv' && e.type === 'response.audio_transcript.delta').map((e) => e.delta).join('')
  const done = events.find((e) => e.dir === 'recv' && e.type === 'response.audio_transcript.done')
  console.log('streamed transcript ==', JSON.stringify(d))
  console.log('transcript.done    ==', JSON.stringify(done?.transcript))
  try { ws.close() } catch {}
  setTimeout(() => process.exit(code), 50)
}
