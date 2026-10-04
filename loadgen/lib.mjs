// Shared plumbing for the loadgen scripts: env config, AEP + portal HTTP
// clients, latency recorder, worker pools, and /metrics histogram diffs.
// Plain Node >=24, no third-party deps.

import fs from 'node:fs';
import path from 'node:path';
import process from 'node:process';

export const PROTOCOL_HEADER = {'X-AEP-Protocol-Version': '1.0'};

export function loadEnv() {
  const env = {...process.env};
  const here = path.dirname(new URL(import.meta.url).pathname);
  const local = path.join(here, '.env.local');
  if (fs.existsSync(local)) {
    for (const line of fs.readFileSync(local, 'utf8').split('\n')) {
      const match = line.match(/^\s*([A-Z_][A-Z0-9_]*)\s*=\s*(.*)\s*$/);
      if (match && !(match[1] in env)) env[match[1]] = match[2];
    }
  }
  for (const key of ['AEP_BASE_URL', 'PORTAL_BASE_URL', 'AEP_ADMIN_USER', 'AEP_ADMIN_PASSWORD']) {
    if (!env[key]) throw new Error(`missing ${key} — create loadgen/.env.local (see loadgen/README.md)`);
  }
  return env;
}

export function parseArgs(argv) {
  const out = {};
  for (let i = 0; i < argv.length; i += 1) {
    const arg = argv[i];
    const match = arg.match(/^--([a-z-]+)(?:=(.*))?$/);
    if (!match) throw new Error(`unknown argument: ${arg}`);
    out[match[1]] = match[2] !== undefined ? match[2] : (argv[i + 1] && !argv[i + 1].startsWith('--') ? argv[(i += 1)] : true);
  }
  return out;
}

const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

function decodeTokenClaims(token) {
  const part = token.split('.')[1];
  return JSON.parse(Buffer.from(part.replace(/-/g, '+').replace(/_/g, '/'), 'base64').toString('utf8'));
}

// ---------------------------------------------------------------- recorder

export class Recorder {
  constructor() {
    this.samples = new Map();
  }

  record(label, ms, ok = true) {
    let entry = this.samples.get(label);
    if (!entry) {
      entry = {latencies: [], errors: 0};
      this.samples.set(label, entry);
    }
    entry.latencies.push(ms);
    if (!ok) entry.errors += 1;
  }

  async measure(label, fn) {
    const start = process.hrtime.bigint();
    let ok = true;
    try {
      return await fn();
    } catch (error) {
      ok = false;
      throw error;
    } finally {
      this.record(label, Number(process.hrtime.bigint() - start) / 1e6, ok);
    }
  }

  snapshot() {
    const out = {};
    for (const [label, entry] of this.samples) {
      const sorted = [...entry.latencies].sort((a, b) => a - b);
      const pick = (q) => sorted[Math.min(sorted.length - 1, Math.floor(q * sorted.length))];
      out[label] = {
        count: sorted.length,
        errors: entry.errors,
        meanMs: Math.round((sorted.reduce((a, b) => a + b, 0) / sorted.length) * 10) / 10,
        p50Ms: Math.round(pick(0.5)),
        p95Ms: Math.round(pick(0.95)),
        p99Ms: Math.round(pick(0.99)),
        maxMs: Math.round(sorted[sorted.length - 1]),
      };
    }
    return out;
  }

  print(title) {
    console.log(`\n== ${title} ==`);
    const rows = Object.entries(this.snapshot());
    if (rows.length === 0) {
      console.log('(no samples)');
      return;
    }
    const head = 'label'.padEnd(46) + 'n'.padStart(6) + 'err'.padStart(5) + 'p50'.padStart(8) + 'p95'.padStart(8) + 'p99'.padStart(8) + 'max'.padStart(9);
    console.log(head);
    for (const [label, s] of rows) {
      console.log(
        label.slice(0, 46).padEnd(46) +
          String(s.count).padStart(6) + String(s.errors).padStart(5) +
          String(s.p50Ms).padStart(8) + String(s.p95Ms).padStart(8) +
          String(s.p99Ms).padStart(8) + String(s.maxMs).padStart(9),
      );
    }
  }

  save(scenario) {
    const dir = path.join(path.dirname(new URL(import.meta.url).pathname), 'results');
    fs.mkdirSync(dir, {recursive: true});
    const stamp = new Date().toISOString().replace(/[:.]/g, '-').slice(0, 19);
    const file = path.join(dir, `${stamp}-${scenario}.json`);
    fs.writeFileSync(file, JSON.stringify({scenario, at: new Date().toISOString(), stats: this.snapshot()}, null, 2));
    console.log(`\n(saved ${file})`);
    return file;
  }
}

// ---------------------------------------------------------------- AEP client

export class AepClient {
  constructor(baseUrl, username, password, recorder = new Recorder()) {
    this.baseUrl = baseUrl.replace(/\/+$/, '');
    this.username = username;
    this.password = password;
    this.recorder = recorder;
    this.accessToken = null;
    this.refreshToken = null;
    this.claims = null;
  }

  async login(label = 'aep:login') {
    return this.recorder.measure(label, async () => {
      const response = await this.#raw('POST', '/aep/v1/auth/password/login', {
        username: this.username,
        password: this.password,
      });
      if (!response.ok) throw new Error(`login ${response.status}: ${JSON.stringify(response.body).slice(0, 200)}`);
      this.accessToken = response.body.accessToken;
      this.refreshToken = response.body.refreshToken ?? null;
      this.claims = decodeTokenClaims(this.accessToken);
      return response.body;
    });
  }

  get deploymentId() {
    return this.claims?.deployment_id ?? this.claims?.deploymentId ?? null;
  }

  async #raw(method, apiPath, body, token) {
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), Number(process.env.LOADGEN_TIMEOUT_MS || 120000));
    try {
      const response = await fetch(this.baseUrl + apiPath, {
        method,
        headers: {
          ...PROTOCOL_HEADER,
          ...(body !== undefined ? {'content-type': 'application/json'} : {}),
          ...(token ? {authorization: `Bearer ${token}`} : {}),
        },
        body: body !== undefined ? JSON.stringify(body) : undefined,
        signal: controller.signal,
      });
      const text = await response.text();
      let parsed = null;
      try {
        parsed = text ? JSON.parse(text) : null;
      } catch {
        parsed = {raw: text.slice(0, 200)};
      }
      return {ok: response.ok, status: response.status, body: parsed};
    } finally {
      clearTimeout(timer);
    }
  }

  // Authenticated call with one automatic re-login on 401.
  async call(method, apiPath, body, {label, query} = {}) {
    const fullPath = apiPath + (query ? '?' + new URLSearchParams(query).toString() : '');
    const tag = label ?? `aep:${method} ${apiPath}`;
    let reAuthed = false;
    return this.recorder.measure(tag, async () => {
      if (!this.accessToken) await this.login();
      let response = await this.#raw(method, fullPath, body, this.accessToken);
      if (response.status === 401 && !reAuthed) {
        reAuthed = true;
        await this.login();
        response = await this.#raw(method, fullPath, body, this.accessToken);
      }
      if (!response.ok) {
        const detail = JSON.stringify(response.body).slice(0, 200);
        throw new Error(`${method} ${apiPath} -> ${response.status} ${detail}`);
      }
      return response.body;
    });
  }

  // Walk a cursor-paginated admin list (envelope key varies: items/teams/...).
  async *walk(adminPath, {limit = 200, label, maxPages = 500} = {}) {
    let cursor;
    for (let page = 0; page < maxPages; page += 1) {
      const query = {limit: String(limit)};
      if (cursor) query.cursor = cursor;
      const body = await this.call('GET', adminPath, undefined, {label, query});
      const key = Object.keys(body).find((k) => Array.isArray(body[k]));
      const items = body[key] ?? [];
      for (const item of items) yield item;
      cursor = body.nextCursor;
      if (!cursor || items.length === 0) return;
    }
  }

  async count(adminPath, label) {
    let n = 0;
    for await (const _ of this.walk(adminPath, {label})) n += 1;
    return n;
  }
}

// ---------------------------------------------------------------- portal client

export class PortalClient {
  constructor(baseUrl, recorder = new Recorder()) {
    this.baseUrl = baseUrl.replace(/\/+$/, '');
    this.recorder = recorder;
    this.cookies = new Map();
    this.csrf = null;
    this.user = null;
  }

  async login(username, password, label = 'portal:login') {
    return this.recorder.measure(label, async () => {
      const response = await fetch(this.baseUrl + '/api/v1/session', {
        method: 'POST',
        headers: {'content-type': 'application/json'},
        body: JSON.stringify({username, password}),
      });
      if (!response.ok) throw new Error(`portal login ${response.status}`);
      for (const cookie of response.headers.getSetCookie()) {
        const [pair] = cookie.split(';');
        const eq = pair.indexOf('=');
        this.cookies.set(pair.slice(0, eq), pair.slice(eq + 1));
      }
      this.csrf = this.cookies.get('csrf_token') ?? null;
      const body = await response.json();
      this.user = body.user;
      return body;
    });
  }

  async call(method, apiPath, body, {label} = {}) {
    const tag = label ?? `portal:${method} ${apiPath}`;
    return this.recorder.measure(tag, async () => {
      const cookie = [...this.cookies.entries()].map(([k, v]) => `${k}=${v}`).join('; ');
      const response = await fetch(this.baseUrl + apiPath, {
        method,
        headers: {
          cookie,
          ...(method !== 'GET' && this.csrf ? {'x-csrf-token': this.csrf} : {}),
          ...(body !== undefined ? {'content-type': 'application/json'} : {}),
        },
        body: body !== undefined ? JSON.stringify(body) : undefined,
      });
      const text = await response.text();
      let parsed = null;
      try {
        parsed = text ? JSON.parse(text) : null;
      } catch {
        parsed = {raw: text.slice(0, 400)};
      }
      if (!response.ok) {
        const error = new Error(`${method} ${apiPath} -> ${response.status} ${JSON.stringify(parsed).slice(0, 200)}`);
        error.status = response.status;
        error.body = parsed;
        throw error;
      }
      return parsed;
    });
  }
}

// ---------------------------------------------------------------- concurrency

export async function pool(size, items, worker) {
  let index = 0;
  const failures = [];
  const runners = Array.from({length: Math.min(size, items.length)}, async () => {
    while (true) {
      const i = index++;
      if (i >= items.length) return;
      try {
        await worker(items[i], i);
      } catch (error) {
        failures.push({item: i, error: String(error.message ?? error).slice(0, 300)});
      }
    }
  });
  await Promise.all(runners);
  return failures;
}

export {sleep};

// ---------------------------------------------------------------- /metrics

// Parse aep_control_service_http_request_duration_seconds histograms into
// {sum,count} keyed by "METHOD ROUTE STATUS" so scenarios can diff before/after.
export async function scrapeAepMetrics(baseUrl) {
  const response = await fetch(baseUrl.replace(/\/+$/, '') + '/metrics');
  const text = await response.text();
  const out = {};
  for (const line of text.split('\n')) {
    const match = line.match(/^aep_control_service_http_request_duration_seconds_(sum|count)\{([^}]*)\}\s+([\d.e+-]+)$/);
    if (!match) continue;
    const labels = {};
    for (const pair of match[2].matchAll(/(\w+)="([^"]*)"/g)) labels[pair[1]] = pair[2];
    const key = `${labels.method ?? ''} ${labels.route ?? ''} ${labels.status ?? ''}`;
    out[key] ??= {sum: 0, count: 0};
    out[key][match[1] === 'sum' ? 'sum' : 'count'] = Number(match[3]);
  }
  return out;
}

export function diffMetrics(before, after) {
  const rows = [];
  for (const [key, value] of Object.entries(after)) {
    const prior = before[key] ?? {sum: 0, count: 0};
    const count = value.count - prior.count;
    if (count <= 0) continue;
    rows.push({route: key, count, avgMs: Math.round(((value.sum - prior.sum) / count) * 1000 * 10) / 10});
  }
  rows.sort((a, b) => b.count - a.count);
  return rows;
}

export function printMetricsDiff(rows, top = 25) {
  console.log('\n-- 服务端 /metrics 差分 (avg per route) --');
  for (const row of rows.slice(0, top)) {
    console.log(`${String(row.count).padStart(7)}x  ${String(row.avgMs).padStart(9)}ms  ${row.route}`);
  }
}

// ---------------------------------------------------------------- state

export function loadState(file = '/tmp/zhiyuan-loadgen-state.json') {
  try {
    return JSON.parse(fs.readFileSync(file, 'utf8'));
  } catch {
    return {};
  }
}

export function saveState(state, file = '/tmp/zhiyuan-loadgen-state.json') {
  fs.writeFileSync(file, JSON.stringify(state, null, 2));
}
