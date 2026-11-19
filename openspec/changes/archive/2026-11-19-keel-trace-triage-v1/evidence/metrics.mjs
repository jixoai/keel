// Metrics with one methodology across the before (original trace) and after
// (equivalent reproduction) traces. Optional marker: events after the
// blink.user_timing TimeStamp of that name are reported as the control window.
import { readFileSync } from 'node:fs'
const file = process.argv[2]
const marker = process.argv[3]
const t = JSON.parse(readFileSync(file, 'utf8'))
const ev = (t.traceEvents ?? t).filter((e) => e.ph !== 'M')

const rendererPids = new Set()
for (const e of ev) if (e.ph === 'M' && e.name === 'process_name' && /Renderer/i.test(e.args?.name ?? '')) rendererPids.add(e.pid)

const times = ev.filter((e) => typeof e.ts === 'number').map((e) => e.ts)
const t0 = Math.min(...times), t1 = Math.max(...times)
let splitTs = Infinity, splitLabel = ''
const mark = ev.find((e) => (e.name === 'TimeStamp' && e.args?.data?.name === marker) || (e.name === 'ClockSync' && e.args?.syncId === marker))
if (mark) { splitTs = mark.ts; splitLabel = marker }
else if (marker === 'mouse-split') {
  const mouse = ev.filter((e) => e.name === 'EventTiming' && /mouse|pointer/.test(e.args?.data?.type ?? '')).map((e) => e.ts)
  if (mouse.length) { splitTs = Math.min(...mouse); splitLabel = 'first mouse/pointer EventTiming' }
}

const inWindow = (e, lo, hi) => e.ts >= lo && e.ts < hi
const busy = (list) => {
  list = list.slice().sort((a, b) => a[0] - b[0])
  let total = 0, s = -1, en = -1
  for (const [a, b] of list) { if (a > en) { if (en > 0) total += en - s; s = a; en = b } else if (b > en) en = b }
  if (en > 0) total += en - s
  return total / 1000
}
const namesOf = (lo, hi) => {
  const m = new Map()
  for (const e of ev) if (inWindow(e, lo, hi)) m.set(e.name, (m.get(e.name) ?? 0) + 1)
  return m
}

function report(lo, hi, label) {
  const win = (ev.filter((e) => inWindow(e, lo, hi)))
  const span = (hi - lo) / 1000
  const name = namesOf(lo, hi)
  const runs = win.filter((e) => e.name === 'RunTask' && e.ph === 'X' && (!rendererPids.size || rendererPids.has(e.pid)))
  const long = runs.filter((e) => e.dur >= 200000)
  const busyMs = busy(runs.map((e) => [e.ts, e.ts + e.dur]))
  const frameState = (e) => e.args?.frame_reporter?.state ?? e.args?.data?.state ?? ''
  const frames = win.filter((e) => e.name === 'PipelineReporter' && /PRESENTED|DROPPED/.test(frameState(e)))
  const dropped = frames.filter((e) => frameState(e) === 'STATE_DROPPED')
  const presented = frames.filter((e) => /PRESENTED/.test(frameState(e)))
  const idleFrames = win.filter((e) => e.name === 'PipelineReporter' && frameState(e) === 'STATE_NO_UPDATE_DESIRED')
  const gc = win.filter((e) => (e.name === 'MinorGC' || e.name === 'MajorGC') && e.ph === 'X')
  const gcMs = gc.reduce((a, e) => a + e.dur, 0) / 1000
  const counters = win.filter((e) => e.name === 'UpdateCounters' && e.ph === 'I' && typeof e.args?.data?.counters?.jsHeapSizeUsed === 'number')
  let heap = null
  if (counters.length > 1) {
    const hs = counters.map((e) => e.args.data.counters.jsHeapSizeUsed)
    let alloc = 0
    for (let i = 1; i < hs.length; i++) if (hs[i] > hs[i - 1]) alloc += hs[i] - hs[i - 1]
    heap = { samples: counters.length, min: (Math.min(...hs) / 1048576).toFixed(1), max: (Math.max(...hs) / 1048576).toFixed(1), grossAllocMBps: (alloc / 1048576 / (span / 1000)).toFixed(1) }
  }
  console.log(`\n== ${label} ==`)
  console.log(`window: ${(lo - t0) / 1000}ms .. ${(hi - t0) / 1000}ms (${span.toFixed(0)}ms)`)
  console.log(`RunTask: ${runs.length} | >=200ms: ${long.length} ${long.map((e) => (e.dur / 1000).toFixed(1)).join(',')}`)
  console.log(`main-thread busy: ${busyMs.toFixed(1)}ms = ${((busyMs / span) * 100).toFixed(1)}%`)
  console.log(`frames: presented ${presented.length} | dropped ${dropped.length} | no-update-desired ${idleFrames.length} (of ${frames.length + idleFrames.length} reported)`)
  console.log(`GC: ${gc.length} (total ${gcMs.toFixed(1)}ms) | MinorGC ${gc.filter((e) => e.name === 'MinorGC').length} / MajorGC ${gc.filter((e) => e.name === 'MajorGC').length}`)
  if (heap) console.log(`heap: ${heap.min}MB..${heap.max}MB over ${heap.samples} samples | gross alloc ~${heap.grossAllocMBps}MB/s`)
  const interesting = ['Paint', 'UpdateLayoutTree', 'Layout', 'ScheduleStyleRecalculation', 'InvalidateLayout', 'AddedModifiedNodeInAnimationFrame', 'RecalcStyle', 'ParseHTML', 'TimerFire', 'Commit', 'EventDispatch']
  for (const n of interesting) console.log(`${n}: ${name.get(n) ?? 0}`)
  const loads = win.filter((e) => e.name === 'EventDispatch' && e.args?.data?.type === 'load')
  console.log(`EventDispatch(load): ${loads.length}`)
  const top = [...name.entries()].sort((a, b) => b[1] - a[1]).slice(0, 12)
  console.log('top:', top.map(([n, c]) => `${n}=${c}`).join(' '))
}

if (splitTs !== Infinity) {
  report(t0, splitTs, 'idle window (after gallery built, before control)')
  report(splitTs, t1, `control window (${splitLabel} … hover)`)
} else {
  report(t0, t1, 'whole trace')
}
