#!/usr/bin/env node
// Language-purity check for the English page tree (M3 evidence, reproducible).
//
// The site ships two parallel trees: English at the root, Chinese under /zh/.
// A Chinese citation that leaks into an English page is a defect, so this
// script walks the built English pages, extracts their rendered text (and,
// separately, the text with <pre>/<code> removed so code samples and inline
// identifiers can be attributed) and counts CJK characters. Any hit is a
// failure; the output is the per-page count list.
//
// Usage: node scripts/check-en-cjk.mjs      (run `pnpm build` first)
//
// The character classes are CJK ideographs plus CJK-only punctuation
// (U+3000–U+303F, U+3400–U+4DBF, U+4E00–U+9FFF, U+FF00–U+FFEF). Typographic
// characters that English prose also uses — em dash, ellipsis, guillemets — are
// deliberately not counted.
import { readFileSync, readdirSync, statSync, existsSync } from 'node:fs'
import { join, relative, resolve, sep } from 'node:path'
import { fileURLToPath } from 'node:url'

const siteRoot = fileURLToPath(new URL('..', import.meta.url))
// Optional argument: check another build (used to compare against an older
// artifact, e.g. `node scripts/check-en-cjk.mjs /tmp/before-dist`).
const distRoot = process.argv[2] ? resolve(process.cwd(), process.argv[2]) : join(siteRoot, 'dist')

const CJK = /[\u3000-\u303f\u3400-\u4dbf\u4e00-\u9fff\uff00-\uffef]/g

function walk(dir, out = []) {
  for (const entry of readdirSync(dir)) {
    const full = join(dir, entry)
    if (statSync(full).isDirectory()) walk(full, out)
    else out.push(full)
  }
  return out
}

const ENTITIES = {
  '&quot;': '"',
  '&#39;': "'",
  '&amp;': '&',
  '&lt;': '<',
  '&gt;': '>',
  '&nbsp;': ' '
}

// Text of the rendered document, optionally with code blocks and inline code
// removed. Only the page body counts: the chrome (nav, language switcher,
// sidebar, footer) and the embedded site data legitimately carry Chinese
// strings, and scripts/styles never count at all.
function pageText(html, { keepCode }) {
  const main = html.match(/<main[\s\S]*?<\/main>/)
  let text = main ? main[0] : html
  if (!main) {
    // The home layout has no <main>: drop the chrome by tag instead.
    text = text.replace(/<header[\s\S]*?<\/header>/gi, '')
    text = text.replace(/<aside[\s\S]*?<\/aside>/gi, '')
    text = text.replace(/<footer[\s\S]*?<\/footer>/gi, '')
  }
  text = text.replace(/<script[\s\S]*?<\/script>/gi, '')
  text = text.replace(/<style[\s\S]*?<\/style>/gi, '')
  if (!keepCode) text = text.replace(/<pre[\s\S]*?<\/pre>/gi, '').replace(/<code[\s\S]*?<\/code>/gi, '')
  text = text.replace(/<[^>]+>/g, ' ')
  text = text.replace(/&[a-z#0-9]+;/gi, (m) => ENTITIES[m] ?? ' ')
  return text
}

function countCjk(text) {
  return (text.match(CJK) ?? []).length
}

if (!existsSync(distRoot)) {
  console.error('dist/ is missing: run `pnpm build` first')
  process.exit(1)
}

let failures = 0
let pages = 0
const report = []
for (const file of walk(distRoot).filter((f) => f.endsWith('.html'))) {
  const rel = relative(distRoot, file).split(sep).join('/')
  if (rel.startsWith('zh/') || rel === '404.html') continue
  pages += 1
  const html = readFileSync(file, 'utf8')
  const prose = countCjk(pageText(html, { keepCode: false }))
  const withCode = countCjk(pageText(html, { keepCode: true }))
  report.push(`${rel}: prose=${prose} incl. code=${withCode}`)
  if (prose > 0 || withCode > 0) {
    failures += 1
    const hits = [...new Set(pageText(html, { keepCode: false }).match(CJK) ?? [])]
    if (hits.length) report.push(`  CJK characters in prose: ${hits.join(' ')}`)
  }
}

for (const line of report) console.log(`  ${line}`)
console.log(`\nEN pages checked: ${pages}`)
console.log(`pages with CJK text: ${failures}`)
process.exit(failures === 0 ? 0 : 1)
