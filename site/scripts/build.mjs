#!/usr/bin/env node
// Build entry point for the site (`pnpm build`).
//
// Steps, in order:
//   1. vitepress build      — fails on dead links (verified: VitePress 1.6
//                             turns internal dead links into build errors)
//   2. bilingual parity     — EN and ZH page sets must be identical
//   3. citation gate        — every `path:line` anchor must resolve
// An error in any step fails the build; GitHub Pages then keeps the previous
// revision (the deploy job never runs).
import { spawn } from 'node:child_process'
import { fileURLToPath } from 'node:url'
import { join } from 'node:path'
import { checkParity } from './parity.mjs'
import { checkCitations } from './check-citations.mjs'
import { checkLinks } from './check-links.mjs'

const siteRoot = fileURLToPath(new URL('..', import.meta.url))
const vitepressBin = join(siteRoot, 'node_modules', 'vitepress', 'bin', 'vitepress.js')

function runVitepress() {
  return new Promise((resolve) => {
    // No positional argument: `vitepress build <dir>` takes a ROOT, not an
    // output directory. The root is the site itself and the output directory
    // comes from `outDir` in .vitepress/config.mts (site/dist).
    const child = spawn(process.execPath, [vitepressBin, 'build'], {
      cwd: siteRoot,
      stdio: ['ignore', 'pipe', 'pipe']
    })
    const chunks = []
    const pipe = (stream, sink) => {
      stream.on('data', (buf) => {
        sink.write(buf)
        chunks.push(buf)
      })
    }
    pipe(child.stdout, process.stdout)
    pipe(child.stderr, process.stderr)
    child.on('close', (code) => {
      const output = Buffer.concat(chunks).toString('utf8')
      resolve({ code, output })
    })
  })
}

console.log('== vitepress build ==')
const built = await runVitepress()
if (built.code !== 0) {
  console.error(`vitepress build failed with exit code ${built.code}`)
  if (/dead link/i.test(built.output)) {
    console.error('dead links are build failures: fix the link or the page')
  }
  process.exit(1)
}

console.log('\n== bilingual parity ==')
const parity = checkParity({ log: (line) => console.log(line) })
if (!parity.ok) {
  console.error('parity check failed: EN/ZH page sets must be identical')
  process.exit(1)
}

console.log('\n== link gate (pages + fragments) ==')
const links = checkLinks({ log: (line) => console.log(line) })
if (!links.ok) {
  console.error('link check failed: a link or fragment points at nothing')
  process.exit(1)
}

console.log('\n== citation gate ==')
const citations = checkCitations({ log: (line) => console.log(line) })
if (!citations.ok) {
  console.error('citation check failed: stale path:line anchors in site pages')
  process.exit(1)
}

console.log(
  `\nSITE BUILD OK — ${parity.en.length} page pairs, ${links.checked} links, ${citations.checked} citations verified`
)
console.log(`output: ${join(siteRoot, 'dist')}`)
