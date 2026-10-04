// Load scenarios against the seeded organization. Each subcommand focuses one
// subsystem; every scenario scrapes AEP /metrics before/after and prints the
// server-side per-route diff alongside client percentiles.

import {AepClient, PortalClient, Recorder, loadEnv, loadState, parseArgs, pool, sleep,
  scrapeAepMetrics, diffMetrics, printMetricsDiff} from './lib.mjs';

const args = parseArgs(process.argv.slice(3));
const scenario = process.argv[2];
const env = loadEnv();
const state = loadState();
const recorder = new Recorder();
const aep = new AepClient(env.AEP_BASE_URL, env.AEP_ADMIN_USER, env.AEP_ADMIN_PASSWORD, recorder);

const CONCURRENCY = Number(args.concurrency ?? 10);
const DURATION_S = Number(args.duration ?? 60);
const AGENTS = Number(args.agents ?? 30);
const seededUsers = Object.keys(state.users ?? {}).filter((u) => u.startsWith('load-u')).sort();
if (seededUsers.length === 0) throw new Error('state 里没有种子用户 — 先跑 seed-org.mjs');
const userPassword = env.SEED_USER_PASSWORD;

let metricsBefore;

function pickUser(i) {
  return seededUsers[i % seededUsers.length];
}

async function agentSession(username, label) {
  const client = new AepClient(env.AEP_BASE_URL, username, userPassword, recorder);
  await client.login(label);
  return client;
}

// ---------------------------------------------------------------- S1

async function listAdmin() {
  await aep.login();
  const lists = [
    ['users', '/aep/v1/admin/users'],
    ['teams', '/aep/v1/admin/teams'],
    ['roles', '/aep/v1/admin/roles'],
    ['models', '/aep/v1/admin/models'],
    ['rules', '/aep/v1/admin/data-scope-rules'],
    ['model-assignments', '/aep/v1/admin/model-assignments'],
    ['skills', '/aep/v1/admin/skills'],
    ['control-events', '/aep/v1/admin/control-events'],
    ['sessions', '/aep/v1/admin/sessions'],
    ['events-search', '/aep/v1/admin/events'],
  ];
  for (const round of [1, 10, 30]) {
    await pool(round, lists, async ([name, path]) => {
      await aep.call('GET', path, undefined, {label: `S1 c${round} ${name}`, query: {limit: '50'}});
    });
  }
  // 数据范围上下文（治理中间件每分钟每用户的真实调用）
  const sample = pickUser(7);
  const userId = await resolveUserId(sample);
  for (let i = 0; i < 20; i += 1) {
    await aep.call('GET', '/aep/v1/admin/data-scope/context', undefined,
      {label: 'S1 data-scope/context', query: {userId}});
  }
}

async function resolveUserId(username) {
  const body = await aep.call('GET', '/aep/v1/admin/users', undefined,
    {label: 'S1 user-lookup', query: {limit: '200'}});
  const key = Object.keys(body).find((k) => Array.isArray(body[k]));
  const hit = (body[key] ?? []).find((u) => u.username === username);
  return hit?.id;
}

// ---------------------------------------------------------------- S2

async function auth() {
  // 登录风暴：不同并发档，每档 CONCURRENCY×3 次独立用户登录
  for (const level of [10, 30, 50]) {
    const targets = Array.from({length: level * 3}, (_, i) => pickUser(i + level * 7));
    await pool(level, targets, async (username) => {
      const client = new AepClient(env.AEP_BASE_URL, username, userPassword, recorder);
      await client.login(`S2 login c${level}`);
    });
  }
  // 预登录 30 个用户做 me/refresh/models/credentials 风
  const clients = await pool(10, Array.from({length: 30}, (_, i) => i), async (i) => {
    const client = await agentSession(pickUser(i), 'S2 prelogin');
    clientsBuffer[i] = client;
  });
  const active = clientsBuffer.filter(Boolean);
  for (const round of [10, 30]) {
    await pool(round, Array.from({length: round * 5}, () => 0), async (_, i) => {
      const client = active[i % active.length];
      await client.call('GET', '/aep/v1/user/me', undefined, {label: `S2 me c${round}`});
    });
    await pool(round, Array.from({length: round * 3}, () => 0), async (_, i) => {
      const client = active[(i + 3) % active.length];
      await client.call('GET', '/aep/v1/user/models', undefined, {label: `S2 models c${round}`});
    });
  }
}

const clientsBuffer = [];

// ---------------------------------------------------------------- S3 events

async function events() {
  await aep.login();
  // 30 个 agent 会话（登录即建会话——会话也是投递扇出的目标）
  const agents = await pool(10, Array.from({length: AGENTS}, (_, i) => i), async (i) => {
    agentsBuffer[i] = await agentSession(pickUser(i), 'S3 agent-login');
  });
  const active = agentsBuffer.filter(Boolean);
  console.log(`agent 会话: ${active.length}`);

  const teamIds = Object.values(state.teams ?? {});
  const userIds = [];
  for await (const user of aep.walk('/aep/v1/admin/users', {label: 'S3 user-walk'})) {
    if (user.username?.startsWith('load-u') && userIds.length < 15) userIds.push(user.id);
  }
  const now = Date.now();
  const mk = (i, scope, type = 'model.catalog.changed') => ({
    type, scope, resource: {type: 'model', id: i % 2 ? 'bench-qwen' : 'bench-anthropic', revision: `load-${now}-${i}`},
    task: {type: 'model.reconcile'}, expiresAt: new Date(now + 2 * 3600e3).toISOString(),
  });
  const plan = [
    ...Array.from({length: 5}, (_, i) => mk(i, {type: 'global'})),
    ...Array.from({length: 10}, (_, i) => mk(i, {type: 'team', id: teamIds[i % teamIds.length]})),
    ...Array.from({length: 15}, (_, i) => mk(i, {type: 'user', id: userIds[i % userIds.length]})),
  ];
  for (const [i, event] of plan.entries()) {
    try {
      const body = await aep.call('POST', '/aep/v1/admin/control-events', event, {label: `S3 create ${event.scope.type}`});
      if (i < 8) console.log(`事件 #${i} scope=${event.scope.type} 扇出会话=${body.deliverySummary?.pending}`);
    } catch (error) {
      console.log(`事件 #${i} 失败: ${String(error.message).slice(0, 140)}`);
    }
  }
  await aep.call('GET', '/aep/v1/admin/control-events', undefined, {label: 'S3 admin-list', query: {limit: '50'}});

  // agent 回路：heartbeat → list → ack → result，60 秒
  const deadline = Date.now() + DURATION_S * 1000;
  let acked = 0;
  let loopErrors = 0;
  await pool(active.length, active, async (client) => {
    let cursor = 0;
    while (Date.now() < deadline) {
      try {
        const beat = await client.call('POST', '/aep/v1/user/heartbeat', {status: 'ok'}, {label: 'S3 heartbeat'});
        if (beat.controlEvents?.pending) {
          const list = await client.call('GET', '/aep/v1/user/control-events', undefined,
            {label: 'S3 list', query: {afterCursor: String(cursor), limit: '50'}});
          for (const item of list.items ?? []) {
            await client.call('POST', `/aep/v1/user/control-events/${item.deliveryId}/acknowledge`,
              {status: 'received'}, {label: 'S3 ack'});
            await client.call('POST', `/aep/v1/user/control-events/${item.deliveryId}/result`,
              {status: 'succeeded'}, {label: 'S3 result'});
            cursor = Math.max(cursor, Number(item.cursor) || 0);
            acked += 1;
          }
        }
      } catch (error) {
        loopErrors += 1;
      }
      await sleep(3000 + Math.random() * 2000);
    }
  });
  console.log(`ack+result 完成: ${acked}（回路错误 ${loopErrors}）`);
  await aep.call('GET', '/aep/v1/admin/control-events', undefined, {label: 'S3 admin-list-after', query: {limit: '50'}});
}

const agentsBuffer = [];

// ---------------------------------------------------------------- S4 telemetry

async function telemetry() {
  const agents = await pool(10, Array.from({length: AGENTS}, (_, i) => i), async (i) => {
    teleBuffer[i] = await agentSession(pickUser(i + 50), 'S4 agent-login');
  });
  const active = teleBuffer.filter(Boolean);
  const rounds = Math.max(3, Math.floor(DURATION_S / 10));
  for (let round = 0; round < rounds; round += 1) {
    await pool(active.length, active, async (client, idx) => {
      const events = Array.from({length: 100}, (_, k) => ({
        eventId: `load-${Date.now()}-${idx}-${round}-${k}`,
        type: ['tool.invoke', 'model.call', 'session.turn'][k % 3],
        occurredAt: new Date().toISOString(),
        resource: {type: 'model', id: 'bench-qwen'},
        result: 'succeeded',
        data: {round, seq: k, note: 'loadgen'},
      }));
      await client.call('POST', '/aep/v1/user/events/batch', {events}, {label: 'S4 batch100'});
    });
  }
  for (const filter of [{type: 'model.call'}, {resourceId: 'bench-qwen'}, {}]) {
    await aep.call('GET', '/aep/v1/admin/events', undefined, {label: 'S4 search', query: {limit: '50', ...filter}});
  }
}

const teleBuffer = [];

// ---------------------------------------------------------------- S5 portal + DE

async function portalDe() {
  await aep.login();
  const admin = new PortalClient(env.PORTAL_BASE_URL, recorder);
  await admin.login(env.AEP_ADMIN_USER, env.AEP_ADMIN_PASSWORD);
  const requester = new PortalClient(env.PORTAL_BASE_URL, recorder);
  await requester.login(pickUser(1), userPassword, 'S5 requester-login');

  // 部门列表 + 成员（>200 截断验证）
  const departments = await admin.call('GET', '/api/v1/departments', undefined, {label: 'S5 departments'});
  const deptList = departments.departments ?? departments.items ?? [];
  console.log(`portal 可见部门: ${deptList.length}`);
  const firstDept = deptList.find((d) => (d.name ?? '').includes('华东')) ?? deptList[0];
  if (firstDept) {
    const members = await admin.call('GET', `/api/v1/departments/${firstDept.id}/members`, undefined, {label: 'S5 members'});
    const memberList = members.members ?? members.items ?? [];
    console.log(`部门 ${firstDept.name}/${firstDept.id} portal 可见成员: ${memberList.length}（AEP 实际约 10 — 截断/过滤行为对照）`);
  }
  const employees = await admin.call('GET', '/api/v1/employees', undefined, {label: 'S5 employees-list'});
  console.log(`现有 DE: ${(employees.employees ?? []).length}`);

  const childIds = Object.entries(state.teams ?? {});
  const teamId = childIds.find(([name]) => name.includes('华东') && name.includes('销售'))?.[1] ?? childIds[0]?.[1];

  // 15 直建 + 15 审批 + 5 驳回
  const total = Number(args.des ?? 30);
  const direct = Math.floor(total / 2);
  const approved = total - direct - 5;
  const created = [];
  for (let i = 1; i <= direct; i += 1) {
    const name = `load-de-${String(i).padStart(2, '0')}`;
    try {
      const out = await admin.call('POST', '/api/v1/employees',
        {name, displayName: `负载员工${String(i).padStart(2, '0')}`, team: teamId, models: ['bench-qwen']},
        {label: 'S5 create-direct'});
      created.push(name);
    } catch (error) { console.log(`直建 ${name}: ${String(error.message).slice(0, 120)}`); }
  }
  for (let i = direct + 1; i <= direct + approved + 5; i += 1) {
    const name = `load-de-${String(i).padStart(2, '0')}`;
    try {
      await requester.call('POST', '/api/v1/employees',
        {name, displayName: `负载员工${String(i).padStart(2, '0')}`, team: teamId, models: ['bench-qwen']},
        {label: 'S5 create-request'});
    } catch (error) { console.log(`申请 ${name}: ${String(error.message).slice(0, 120)}`); }
  }
  const pending = await admin.call('GET', '/api/v1/requests', undefined, {label: 'S5 requests-pending', query: {state: 'pending', limit: '100'}});
  const requestList = pending.requests ?? pending.items ?? [];
  console.log(`待审批: ${requestList.length}`);
  for (const [i, req] of requestList.entries()) {
    const decision = i < approved ? 'approve' : 'reject';
    await admin.call('POST', `/api/v1/requests/${req.id}/${decision}`, {}, {label: `S5 ${decision}`});
  }
  // Ready 延迟统计
  const started = Date.now();
  const ready = {};
  const deadline = Date.now() + 600_000;
  const names = Array.from({length: total}, (_, i) => `load-de-${String(i + 1).padStart(2, '0')}`);
  while (Date.now() < deadline && Object.keys(ready).length < total) {
    for (const name of names) {
      if (ready[name]) continue;
      try {
        const out = await admin.call('GET', `/api/v1/employees/${name}`, undefined, {label: 'S5 poll-employee'});
        const phase = out.employee?.phase ?? out.phase;
        if (phase === 'Ready' || phase === 'Failed') {
          ready[name] = {phase, seconds: Math.round((Date.now() - started) / 1000)};
        }
      } catch { /* 尚未创建 */ }
    }
    await sleep(5000);
  }
  const latencies = Object.values(ready).map((v) => v.seconds).sort((a, b) => a - b);
  const failed = Object.values(ready).filter((v) => v.phase === 'Failed').length;
  console.log(`DE Ready 延迟: min=${latencies[0] ?? '-'}s p50=${latencies[Math.floor(latencies.length / 2)] ?? '-'}s max=${latencies[latencies.length - 1] ?? '-'}s；Failed=${failed}；未完成=${total - latencies.length}`);
}

// ---------------------------------------------------------------- S6 chat

async function chat() {
  const portal = new PortalClient(env.PORTAL_BASE_URL, recorder);
  await portal.login(env.AEP_ADMIN_USER, env.AEP_ADMIN_PASSWORD);
  const target = args.employee ?? 'sales-helper';
  await portal.call('POST', '/api/v1/session/employee', {name: target}, {label: 'S6 select-employee'});
  const cookie = () => [...portal.cookies.entries()].map(([k, v]) => `${k}=${v}`).join('; ');
  const base = `${env.PORTAL_BASE_URL}/api/v1/employees/${target}/chat/api/threads`;
  // 建线程（LangGraph 协议：threads → {thread_id}/runs/stream SSE）
  const thread = await portal.call('POST', `/api/v1/employees/${target}/chat/api/threads`, {}, {label: 'S6 create-thread'});
  const threadId = thread.thread_id ?? thread.id ?? thread.threadId;
  const rounds = Number(args.rounds ?? 2);
  for (let i = 0; i < rounds; i += 1) {
    await recorder.measure(i === 0 ? 'S6 chat 冷唤醒+首轮 SSE' : 'S6 chat 热轮 SSE', async () => {
      const response = await fetch(`${base}/${threadId}/runs/stream`, {
        method: 'POST',
        headers: {cookie: cookie(), 'x-csrf-token': portal.csrf, 'content-type': 'application/json'},
        body: JSON.stringify({
          input: {messages: [{role: 'user', content: `负载测试第${i + 1}轮，请只回复两个字：收到`}]},
          stream_mode: 'values',
        }),
      });
      if (!response.ok) throw new Error(`chat ${response.status}: ${(await response.text()).slice(0, 120)}`);
      const reader = response.body.getReader();
      let bytes = 0;
      while (true) {
        const {done, value} = await reader.read();
        if (done) break;
        bytes += value.length;
      }
      return bytes;
    });
  }
}

// ---------------------------------------------------------------- S7 soak

async function soak() {
  await aep.login();
  const me = await agentSession(pickUser(0), 'S7 prelogin');
  const deadline = Date.now() + DURATION_S * 1000;
  const mix = [
    [0.3, async () => aep.call('GET', '/aep/v1/admin/users', undefined, {label: 'S7 users', query: {limit: '50'}})],
    [0.25, async () => me.call('GET', '/aep/v1/user/me', undefined, {label: 'S7 me'})],
    [0.2, async () => me.call('GET', '/aep/v1/user/models', undefined, {label: 'S7 models'})],
    [0.15, async () => aep.call('GET', '/aep/v1/admin/teams', undefined, {label: 'S7 teams', query: {limit: '50'}})],
    [0.1, async () => {
      const c = new AepClient(env.AEP_BASE_URL, pickUser(Math.floor(Math.random() * 300)), userPassword, recorder);
      await c.login('S7 login');
    }],
  ];
  let ops = 0;
  await pool(CONCURRENCY, Array.from({length: CONCURRENCY}, () => 0), async () => {
    while (Date.now() < deadline) {
      const dice = Math.random();
      let acc = 0;
      for (const [weight, fn] of mix) {
        acc += weight;
        if (dice <= acc) { await fn(); ops += 1; break; }
      }
      await sleep(50 + Math.random() * 100);
    }
  });
  console.log(`soak 完成: ${ops} ops / ${DURATION_S}s = ${(ops / DURATION_S).toFixed(1)} rps`);
}

// ---------------------------------------------------------------- dispatch

const SCENARIOS = {listAdmin, auth, events, telemetry, portalDe, chat, soak};
const kebab = scenario?.replace(/-([a-z])/g, (_, c) => c.toUpperCase());

async function main() {
  if (!scenario || !SCENARIOS[kebab]) {
    console.log(`用法: node loadgen/scenarios.mjs <list-admin|auth|events|telemetry|portal-de|chat|soak> [--concurrency N] [--duration S] [--agents N] [--des N] [--employee name] [--rounds N]`);
    return;
  }
  metricsBefore = await scrapeAepMetrics(env.AEP_BASE_URL);
  await SCENARIOS[kebab]();
  recorder.save(kebab);
  recorder.print(kebab);
  const after = await scrapeAepMetrics(env.AEP_BASE_URL);
  printMetricsDiff(diffMetrics(metricsBefore, after));
}

main().catch((error) => {
  console.error('场景失败:', error.message);
  process.exitCode = 1;
});
