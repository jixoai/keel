#!/usr/bin/env node
// Link gate for the built site.
//
// VitePress fails the build on internal dead links found in markdown, but it
// does NOT validate URL fragments (`/core/#section`): a link to a heading that
// does not exist builds clean and silently lands at the top of the page. This
// script closes that hole by walking the built HTML: every internal href must
// resolve to a generated file, and every fragment must exist as an element id
// in the target page.
import { readFileSync, readdirSync, statSync, existsSync } from 'node:fs'
import { join, relative, sep } from 'node:path'
import { fileURLToPath } from 'node:url'

const siteRoot = fileURLToPath(new URL('..', import.meta.url))
const distRoot = join(siteRoot, 'dist')

function walk(dir, out = []) {
  for (const entry of readdirSync(dir)) {
    const full = join(dir, entry)
    if (statSync(full).isDirectory()) walk(full, out)
    else out.push(full)
  }
  return out
}

// Resolve a site-absolute path (as written in the HTML) to a built file.
function resolvePage(pathname) {
  const candidates = []
  if (pathname.endsWith('/')) {
    candidates.push(join(distRoot, pathname, 'index.html'))
  } else {
    candidates.push(join(distRoot, `${pathname}.html`))
    candidates.push(join(distRoot, pathname, 'index.html'))
    candidates.push(join(distRoot, pathname))
  }
  for (const c of candidates) if (existsSync(c) && statSync(c).isFile()) return c
  return null
}

export function checkLinks({ log = console.log } = {}) {
  if (!existsSync(distRoot)) {
    log('dist/ is missing: run the vitepress build first')
    return { ok: false, checked: 0, problems: ['dist/ missing'] }
  }
  const htmlFiles = walk(distRoot).filter((f) => f.endsWith('.html'))
  const idsCache = new Map()
  const problems = []
  let checked = 0

  const idsOf = (file) => {
    if (!idsCache.has(file)) {
      const text = readFileSync(file, 'utf8')
      idsCache.set(file, new Set([...text.matchAll(/\sid="([^"]+)"/g)].map((m) => m[1])))
    }
    return idsCache.get(file)
  }

  for (const file of htmlFiles) {
    const text = readFileSync(file, 'utf8')
    const from = relative(distRoot, file).split(sep).join('/')
    for (const match of text.matchAll(/href="([^"]+)"/g)) {
      const href = match[1]
      if (/^(https?:|mailto:|data:|javascript:|\/\/)/.test(href)) continue
      checked += 1
      let pathname = href
      let fragment = ''
      const hashAt = href.indexOf('#')
      if (hashAt >= 0) {
        pathname = href.slice(0, hashAt)
        fragment = decodeURIComponent(href.slice(hashAt + 1))
      }
      const target = pathname === '' ? file : pathname.startsWith('/') ? resolvePage(pathname) : join(file, '..', pathname)
      if (!target || !existsSync(target)) {
        problems.push(`${from}: ${href} — target page not generated`)
        continue
      }
      if (fragment) {
        const ids = idsOf(target)
        if (!ids.has(fragment) && !ids.has(decodeURIComponent(fragment))) {
          problems.push(`${from}: ${href} — no element id "${fragment}" in ${relative(distRoot, target)}`)
        }
      }
    }
  }

  log(`internal links checked: ${checked}`)
  for (const p of problems) log(`BROKEN ${p}`)
  // A handful of duplicates are normal (nav + footer + in-page links).
  return { ok: problems.length === 0, checked, problems }
}

const isDirectRun = process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]
if (isDirectRun) {
  const { ok, checked } = checkLinks()
  console.log(ok ? `LINKS OK (${checked})` : 'LINKS FAILED')
  process.exit(ok ? 0 : 1)
}
