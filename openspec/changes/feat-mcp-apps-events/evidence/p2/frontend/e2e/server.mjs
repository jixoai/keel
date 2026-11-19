import http from 'node:http';
import fs from 'node:fs';
import path from 'node:path';
const ROOT = '/Users/kzf/Development/GitHub/jixoai/fde2e/.worktrees/keel-task-mcp-p2fe/web/dist/web/keel/Keel';
const APP_ID = 'mcpapp_0123456789abcdef';
const HASH = 'a'.repeat(64);
const APP_HTML = fs.readFileSync('/tmp/p2fe-e2e/fake-app.html', 'utf8');
const LOG = '/tmp/p2fe-e2e/server-headers.log';
fs.writeFileSync(LOG, '');
const MIME = { '.html':'text/html; charset=utf-8', '.js':'text/javascript', '.css':'text/css', '.json':'application/json', '.png':'image/png', '.wasm':'application/wasm', '.bin':'application/octet-stream', '.webmanifest':'application/manifest+json', '.svg':'image/svg+xml' };
const log = (line) => fs.appendFileSync(LOG, line + '\n');
const server = http.createServer((req, res) => {
  const url = new URL(req.url, 'http://127.0.0.1:13679');
  if (url.pathname === `/v1/mcp/apps/${APP_ID}/view`) {
    const headers = {
      'Content-Type': 'text/html; charset=utf-8',
      'X-MCP-App-Mime': 'text/html;profile=mcp-app',
      'X-MCP-App-Hash': HASH,
      'Content-Security-Policy': "default-src 'none'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'self'; object-src 'none'; base-uri 'self'",
      'X-Content-Type-Options': 'nosniff',
      'Referrer-Policy': 'no-referrer',
      'Cache-Control': 'public, max-age=31536000, immutable',
      'Set-Cookie': 'keel_host_secret=1; Path=/; SameSite=Lax',
    };
    res.writeHead(200, headers);
    log(`APP-DOC ${req.method} ${req.url}`);
    for (const [k, v] of Object.entries(headers)) log(`  ${k}: ${v}`);
    res.end(APP_HTML);
    return;
  }
  if (url.pathname === '/evil.html') {
    res.writeHead(200, { 'Content-Type': 'text/html; charset=utf-8' });
    log('EVIL GET /evil.html');
    res.end(fs.readFileSync('/tmp/p2fe-e2e/evil.html', 'utf8'));
    return;
  }
  if (url.pathname === '/evil-report') {
    let body = '';
    req.on('data', (chunk) => { body += chunk; });
    req.on('end', () => { log('EVIL-REPORT ' + body.slice(0, 2000)); res.writeHead(204); res.end(); });
    return;
  }
  if (url.pathname === '/cookie-check') {
    res.writeHead(200, { 'Content-Type': 'application/json', 'Access-Control-Allow-Origin': 'null', 'Access-Control-Allow-Credentials': 'true', 'Vary': 'Origin' });
    log(`COOKIE-CHECK cookie=${JSON.stringify(req.headers.cookie ?? '')}`);
    res.end(JSON.stringify({ cookie: req.headers.cookie ?? '' }));
    return;
  }
  let file = path.join(ROOT, url.pathname === '/' ? 'index.html' : url.pathname);
  if (!file.startsWith(ROOT)) { res.writeHead(403); res.end(); return; }
  if (!fs.existsSync(file) || fs.statSync(file).isDirectory()) {
    // SPA fallback, but never for /v1 or /assets.
    if (url.pathname.startsWith('/v1/') || url.pathname.startsWith('/assets/') || path.extname(url.pathname)) {
      res.writeHead(404); res.end('not found'); return;
    }
    file = path.join(ROOT, 'index.html');
  }
  res.writeHead(200, { 'Content-Type': MIME[path.extname(file)] ?? 'application/octet-stream' });
  fs.createReadStream(file).pipe(res);
});
server.listen(13679, '127.0.0.1', () => console.log('e2e server on 13679'));
