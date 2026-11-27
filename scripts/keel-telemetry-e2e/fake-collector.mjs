#!/usr/bin/env node
// Standalone OTLP/HTTP JSON collector for the keel.telemetry live E2E.
//
// It is intentionally independent of keel: the exporter under test posts real
// OTLP JSON to /v1/{traces,metrics,logs}, and every request is appended to a
// JSONL evidence file with its parsed body. Sensitive header values
// (authorization/cookie/token/secret/api-key) are never written: the record
// carries a SHA-256 fingerprint and the length instead, which is enough to
// prove the configured header arrived without leaking the secret into evidence.
//
//   COLLECTOR_LOG=/path/requests.jsonl node fake-collector.mjs <port>
//
// GET /health answers {ok, requests}; POST /v1/{traces,metrics,logs} answers
// 200 with an OTLP partial-success body. No TLS, no auth: loopback only.
import crypto from 'node:crypto';
import fs from 'node:fs';
import http from 'node:http';

const PORT = Number(process.argv[2] || process.env.COLLECTOR_PORT || 13730);
const LOG = process.env.COLLECTOR_LOG || '';
if (!LOG) {
  console.error('COLLECTOR_LOG is required');
  process.exit(2);
}
fs.writeFileSync(LOG, '');

const MAX_BODY = 32 << 20;
const SENSITIVE = /authorization|cookie|token|secret|api[-_]?key/i;
const PATHS = new Set(['/v1/traces', '/v1/metrics', '/v1/logs']);
let requests = 0;

const sha256 = (value) => crypto.createHash('sha256').update(value).digest('hex');

function attributeMap(attributes) {
  const out = {};
  for (const item of Array.isArray(attributes) ? attributes : []) {
    const value = item?.value || {};
    if ('stringValue' in value) out[item.key] = value.stringValue;
    else if ('intValue' in value) out[item.key] = value.intValue;
    else if ('doubleValue' in value) out[item.key] = value.doubleValue;
    else if ('boolValue' in value) out[item.key] = value.boolValue;
    else if ('arrayValue' in value) out[item.key] = (value.arrayValue?.values || []).map((entry) => entry?.stringValue ?? entry?.intValue ?? entry?.doubleValue ?? null);
    else out[item.key] = null;
  }
  return out;
}

function resourceAttributes(resource) {
  return attributeMap(resource?.attributes);
}

function summarizeTraces(body) {
  const spans = [];
  for (const resourceSpan of body?.resourceSpans || []) {
    for (const scopeSpan of resourceSpan?.scopeSpans || []) {
      for (const span of scopeSpan?.spans || []) {
        spans.push({
          name: span?.name,
          traceId: span?.traceId,
          spanId: span?.spanId,
          parentSpanId: span?.parentSpanId || null,
          status: span?.status?.code,
          attributes: attributeMap(span?.attributes),
          events: (span?.events || []).map((event) => ({ name: event?.name, attributes: attributeMap(event?.attributes) })),
        });
      }
    }
  }
  return {
    kind: 'traces',
    resource: resourceAttributes(body?.resourceSpans?.[0]?.resource),
    spans,
  };
}

function summarizeMetrics(body) {
  const metrics = [];
  for (const resourceMetric of body?.resourceMetrics || []) {
    for (const scopeMetric of resourceMetric?.scopeMetrics || []) {
      for (const metric of scopeMetric?.metrics || []) {
        const sum = metric?.sum;
        const histogram = metric?.histogram;
        const points = (sum?.dataPoints || histogram?.dataPoints || []).map((point) => ({
          attributes: attributeMap(point?.attributes),
          value: point?.asDouble ?? point?.asInt ?? point?.sum ?? null,
          count: point?.count ?? null,
        }));
        metrics.push({
          name: metric?.name,
          unit: metric?.unit || '',
          kind: sum ? 'sum' : histogram ? 'histogram' : 'unknown',
          monotonic: sum?.isMonotonic ?? null,
          temporality: sum?.aggregationTemporality ?? histogram?.aggregationTemporality ?? null,
          points,
        });
      }
    }
  }
  return {
    kind: 'metrics',
    resource: resourceAttributes(body?.resourceMetrics?.[0]?.resource),
    metrics,
  };
}

function summarizeLogs(body) {
  const records = [];
  for (const resourceLog of body?.resourceLogs || []) {
    for (const scopeLog of resourceLog?.scopeLogs || []) {
      for (const record of scopeLog?.logRecords || []) {
        records.push({
          body: record?.body?.stringValue ?? null,
          severityText: record?.severityText ?? null,
          severityNumber: record?.severityNumber ?? null,
          attributes: attributeMap(record?.attributes),
        });
      }
    }
  }
  return {
    kind: 'logs',
    resource: resourceAttributes(body?.resourceLogs?.[0]?.resource),
    records,
  };
}

function summarize(path, body) {
  if (path === '/v1/traces') return summarizeTraces(body);
  if (path === '/v1/metrics') return summarizeMetrics(body);
  return summarizeLogs(body);
}

function headersOf(req) {
  const out = {};
  for (const [name, raw] of Object.entries(req.headers)) {
    if (raw === undefined) continue;
    const value = Array.isArray(raw) ? raw.join(', ') : String(raw);
    if (SENSITIVE.test(name)) {
      out[name] = { redacted: true, sha256: sha256(value), length: value.length };
    } else {
      out[name] = value;
    }
  }
  return out;
}

function readBody(req) {
  return new Promise((resolve, reject) => {
    const chunks = [];
    let size = 0;
    req.on('data', (chunk) => {
      size += chunk.length;
      if (size > MAX_BODY) {
        reject(new Error('body too large'));
        req.destroy();
        return;
      }
      chunks.push(chunk);
    });
    req.on('end', () => resolve(Buffer.concat(chunks).toString('utf8')));
    req.on('error', reject);
  });
}

function respond(res, status, value) {
  const body = JSON.stringify(value);
  res.writeHead(status, {
    'Content-Type': 'application/json',
    'Content-Length': Buffer.byteLength(body),
  });
  res.end(body);
}

const server = http.createServer(async (req, res) => {
  const path = (req.url || '').split('?')[0];
  if (req.method === 'GET' && path === '/health') {
    respond(res, 200, { ok: true, requests, log: LOG });
    return;
  }
  if (req.method !== 'POST' || !PATHS.has(path)) {
    respond(res, 404, { error: 'not an OTLP signal path' });
    return;
  }
  let raw = '';
  try {
    raw = await readBody(req);
  } catch (error) {
    respond(res, 413, { error: String(error?.message || error) });
    return;
  }
  let body = null;
  try {
    body = raw ? JSON.parse(raw) : {};
  } catch (error) {
    respond(res, 400, { error: 'invalid JSON: ' + String(error?.message || error) });
    return;
  }
  requests += 1;
  const record = {
    seq: requests,
    receivedAt: new Date().toISOString(),
    method: req.method,
    path,
    remote: req.socket?.remoteAddress || '',
    contentType: req.headers['content-type'] || '',
    contentLength: raw.length,
    headers: headersOf(req),
    summary: summarize(path, body),
    body,
  };
  fs.appendFileSync(LOG, JSON.stringify(record) + '\n');
  const kind = record.summary.kind;
  const detail =
    kind === 'traces' ? `${record.summary.spans.length} span(s)`
      : kind === 'metrics' ? `${record.summary.metrics.length} metric(s)`
        : `${record.summary.records.length} log record(s)`;
  console.log(`[collector] ${path} ${raw.length} bytes ${detail} auth=${record.headers.authorization ? 'sha256:' + String(record.headers.authorization.sha256).slice(0, 12) : 'none'}`);
  respond(res, 200, { partialSuccess: {} });
});

server.listen(PORT, '127.0.0.1', () => {
  console.log(`fake OTLP collector listening on 127.0.0.1:${PORT} (evidence ${LOG})`);
});

function shutdown(signal) {
  console.log(`[collector] ${signal}; stopping with ${requests} request(s) recorded`);
  server.close(() => process.exit(0));
  setTimeout(() => process.exit(0), 1500).unref();
}
process.on('SIGTERM', () => shutdown('SIGTERM'));
process.on('SIGINT', () => shutdown('SIGINT'));
