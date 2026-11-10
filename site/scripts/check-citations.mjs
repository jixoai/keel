#!/usr/bin/env node
// Citation gate.
//
// Requirement: every technical claim that names a code location carries a
// `path:line` anchor. This script extracts those anchors from the site pages
// and verifies that (a) the file exists in the repository and (b) the line
// number is inside the file. A stale anchor fails the build, exactly like a
// stale fact.
//
// Only repository-relative paths that live in known source roots are checked;
// prose references to files without a line number are ignored on purpose.
import { readFileSync, readdirSync, statSync, existsSync } from 'node:fs'
import { join, relative, sep } from 'node:path'
import { fileURLToPath } from 'node:url'

const siteRoot = fileURLToPath(new URL('..', import.meta.url))
const repoRoot = join(siteRoot, '..')
const docsRoot = join(siteRoot, 'docs')

const ROOTS = ['internal', 'cmd', 'docs', 'scripts', 'openspec', 'web', 'packages']
const EXT = ['go', 'sql', 'md', 'json', 'yml', 'yaml', 'ts', 'tsx', 'mjs']
const PATTERN = new RegExp(
  String.raw`\b((?:${ROOTS.join('|')})/[\w./-]+\.(?:${EXT.join('|')})):(\d+)\b`,
  'g'
)

function walk(dir, out = []) {
  for (const entry of readdirSync(dir)) {
    const full = join(dir, entry)
    const st = statSync(full)
    if (st.isDirectory()) walk(full, out)
    else out.push(full)
  }
  return out
}

export function checkCitations({ log = console.log } = {}) {
  const files = walk(docsRoot).filter((f) => f.endsWith('.md'))
  const problems = []
  let checked = 0

  for (const file of files) {
    const text = readFileSync(file, 'utf8')
    const rel = relative(siteRoot, file).split(sep).join('/')
    for (const match of text.matchAll(PATTERN)) {
      const [, path, lineRaw] = match
      const line = Number(lineRaw)
      const target = join(repoRoot, path)
      checked += 1
      if (!existsSync(target)) {
        problems.push(`${rel}: ${path}:${line} — file does not exist`)
        continue
      }
      const lines = readFileSync(target, 'utf8').split('\n')
      if (line < 1 || line > lines.length) {
        problems.push(`${rel}: ${path}:${line} — file has only ${lines.length} lines`)
        continue
      }
      // A citation pointing at a blank line means the anchor drifted (the file
      // was edited and the cited content moved). That is exactly the failure
      // this gate exists to catch, so it fails rather than passing silently.
      if (lines[line - 1].trim() === '') {
        problems.push(`${rel}: ${path}:${line} — cites a blank line (anchor drifted?)`)
      }
    }
  }

  log(`citations checked: ${checked}`)
  for (const p of problems) log(`STALE ${p}`)
  return { ok: problems.length === 0, checked, problems }
}

const isDirectRun = process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]
if (isDirectRun) {
  const { ok, checked } = checkCitations()
  console.log(ok ? `CITATIONS OK (${checked})` : 'CITATIONS FAILED')
  process.exit(ok ? 0 : 1)
}
