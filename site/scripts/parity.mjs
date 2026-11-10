#!/usr/bin/env node
// Bilingual parity gate.
//
// The site ships two fully parallel trees: English at the root and Chinese
// under /zh/. This script asserts the page sets are identical (same relative
// path, same file count) and that every page exists on both sides. It is
// wired into `pnpm build` and can be run alone with `pnpm parity`.
import { readdirSync, statSync } from 'node:fs'
import { join, relative, sep } from 'node:path'
import { fileURLToPath } from 'node:url'

const siteRoot = fileURLToPath(new URL('..', import.meta.url))
const docsRoot = join(siteRoot, 'docs')
const zhRoot = join(docsRoot, 'zh')

function walk(dir, out = []) {
  for (const entry of readdirSync(dir)) {
    const full = join(dir, entry)
    const st = statSync(full)
    if (st.isDirectory()) walk(full, out)
    else out.push(full)
  }
  return out
}

function pagesUnder(root) {
  return walk(root)
    .filter((f) => f.endsWith('.md'))
    .map((f) => relative(root, f).split(sep).join('/'))
    .sort()
}

export function checkParity({ log = console.log } = {}) {
  const en = pagesUnder(docsRoot).filter((p) => !p.startsWith('zh/'))
  const zh = pagesUnder(zhRoot)

  const enSet = new Set(en)
  const zhSet = new Set(zh)
  const missingZh = en.filter((p) => !zhSet.has(p))
  const missingEn = zh.filter((p) => !enSet.has(p))

  log(`EN pages: ${en.length}`)
  log(`ZH pages: ${zh.length}`)
  for (const p of en) log(`  /${p.replace(/\.md$/, '')}  ⟷  /zh/${p.replace(/\.md$/, '')}`)

  let ok = true
  if (missingZh.length) {
    ok = false
    log(`MISSING in /zh/: ${missingZh.join(', ')}`)
  }
  if (missingEn.length) {
    ok = false
    log(`MISSING in root (en): ${missingEn.join(', ')}`)
  }
  if (en.length !== zh.length) {
    ok = false
    log(`COUNT MISMATCH: en=${en.length} zh=${zh.length}`)
  }
  return { ok, en, zh }
}

const isDirectRun = process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]
if (isDirectRun) {
  const { ok } = checkParity()
  console.log(ok ? 'PARITY OK' : 'PARITY FAILED')
  process.exit(ok ? 0 : 1)
}
