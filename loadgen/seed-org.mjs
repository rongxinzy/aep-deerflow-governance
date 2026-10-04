// Seed a large organization into the deployed AEP: roles, a 3-root team tree,
// N users via the bulk import endpoint, data-scope rules, model assignments,
// and a few skills. Idempotent with --resume (rebuilds id maps from lists).

import {AepClient, Recorder, loadEnv, loadState, saveState, parseArgs, sleep} from './lib.mjs';

const args = parseArgs(process.argv.slice(2));
const USERS = Number(args.users ?? 300);
const ROOTS = Number(args.roots ?? 3);
const TEAMS_PER_ROOT = Number(args['teams-per-root'] ?? 10);
const ROLES = Number(args.roles ?? 10);
const RULES = Number(args.rules ?? 40);
const RESUME = Boolean(args.resume);

const env = loadEnv();
const recorder = new Recorder();
const aep = new AepClient(env.AEP_BASE_URL, env.AEP_ADMIN_USER, env.AEP_ADMIN_PASSWORD, recorder);
const state = RESUME ? loadState() : {};
if (!state.roles) state.roles = {};
if (!state.teams) state.teams = {};
if (!state.users) state.users = {};

// Contract probe: pre-server-id deployments (control-service < 0.7.0) reject
// creates without a client-supplied id. One probe decides for the whole run.
async function detectLegacyContract() {
  try {
    const body = await aep.call('POST', '/aep/v1/admin/roles', {name: 'loadgen-contract-probe'}, {label: 'seed:contract-probe'});
    await aep.call('DELETE', `/aep/v1/admin/roles/${body.id}`, undefined, {label: 'seed:contract-probe-cleanup'});
    return false;
  } catch (error) {
    if (String(error.message).includes('-> 400')) return true;
    throw error;
  }
}

const REGION = ['华东', '华北', '华南'];
const FUNCTION = ['销售', '研发', '财务', '生产', '人力', '客服', '法务', '采购', '市场', '运营'];

async function ensureRole(name, permissions, legacyId) {
  if (state.roles[name]) return state.roles[name];
  const body = await aep.call('POST', '/aep/v1/admin/roles', {
    ...(legacyId ? {id: legacyId} : {}),
    name, description: `负载测试角色 ${name}`, permissions,
  }, {label: 'seed:role-create'});
  state.roles[name] = body.role?.id ?? body.id;
  return state.roles[name];
}

async function ensureTeam(name, parentId, legacyId) {
  if (state.teams[name]) return state.teams[name];
  const body = await aep.call('POST', '/aep/v1/admin/teams', {
    ...(legacyId ? {id: legacyId} : {}),
    name, description: `负载测试部门 ${name}`, ...(parentId ? {parentId} : {}),
  }, {label: 'seed:team-create'});
  state.teams[name] = body.team?.id ?? body.id;
  return state.teams[name];
}

async function main() {
  await aep.login('seed:admin-login');
  const legacy = await detectLegacyContract();
  console.log(`deployment=${aep.claims?.deployment_id} 契约=${legacy ? '旧版(客户端 id)' : '新版(服务端 slug)'} — 目标: ${ROLES} 角色 / ${ROOTS * TEAMS_PER_ROOT + ROOTS} 部门 / ${USERS} 用户 / ${RULES} 数据范围规则`);

  if (RESUME) {
    for await (const team of aep.walk('/aep/v1/admin/teams', {label: 'seed:walk-teams'})) {
      if (!state.teams[team.name]) state.teams[team.name] = team.id;
    }
    for await (const role of aep.walk('/aep/v1/admin/roles', {label: 'seed:walk-roles'})) {
      if (!state.roles[role.name]) state.roles[role.name] = role.id;
    }
    console.log(`resume: 已有 ${Object.keys(state.teams).length} 部门 / ${Object.keys(state.roles).length} 角色的 id 映射`);
  }

  // ---- 1. roles (with real permission ids sampled from the catalog) ----
  const permissions = await aep.call('GET', '/aep/v1/admin/permissions', undefined, {label: 'seed:permissions-list'});
  const permIds = (permissions.permissions ?? []).map((p) => p.id);
  console.log(`权限目录: ${permIds.length} 个`);
  const roleNames = [];
  for (let i = 0; i < ROLES; i += 1) {
    const name = `负载-${['高管', '经理', '主管', '专员', '审计', '合规', '访客', '外包', '实习', '运营'][i % 10]}${String(i + 1).padStart(2, '0')}`;
    roleNames.push(name);
    const picks = [permIds[i % permIds.length], permIds[(i + 7) % permIds.length], permIds[(i + 13) % permIds.length]];
    await ensureRole(name, picks, legacy ? `load-role-${String(i + 1).padStart(2, '0')}` : undefined);
  }
  console.log(`角色就绪: ${roleNames.length}`);

  // ---- 2. team tree: roots then children ----
  const childTeams = [];
  for (let r = 0; r < ROOTS; r += 1) {
    const rootName = `负载-${REGION[r % REGION.length]}集团`;
    const rootId = await ensureTeam(rootName, undefined, legacy ? `load-team-root-${r + 1}` : undefined);
    for (let c = 0; c < TEAMS_PER_ROOT; c += 1) {
      const name = `负载-${REGION[r % REGION.length]}${FUNCTION[c % FUNCTION.length]}部`;
      await ensureTeam(name, rootId, legacy ? `load-team-r${r + 1}f${c + 1}` : undefined);
      childTeams.push(name);
    }
  }
  const childIds = childTeams.map((n) => state.teams[n]);
  const rootNames = [0, 1, 2].slice(0, ROOTS).map((r) => `负载-${REGION[r % REGION.length]}集团`);
  console.log(`部门树就绪: ${ROOTS} 根 + ${childTeams.length} 子部门`);

  // ---- 3. bulk user import ----
  const seedPassword = env.SEED_USER_PASSWORD;
  if (!seedPassword || seedPassword.length < 12) throw new Error('SEED_USER_PASSWORD 至少 12 字符（密码策略）');
  let seededUsers = 0;
  let highestSeq = 0;
  for await (const user of aep.walk('/aep/v1/admin/users', {label: 'seed:count-users'})) {
    if (user.username?.startsWith('load-u')) {
      seededUsers += 1;
      highestSeq = Math.max(highestSeq, Number(user.username.slice(6)) || 0);
    }
  }
  const remaining = Math.max(0, USERS - seededUsers);
  if (remaining > 0) {
    const batch = [];
    for (let i = 1; i <= remaining; i += 1) {
      const seq = String(highestSeq + i).padStart(4, '0');
      const username = `load-u${seq}`;
      const rolePrimary = state.roles[roleNames[i % ROLES]];
      const roleSecond = i % 3 === 0 ? state.roles[roleNames[(i + 1) % ROLES]] : null;
      const teamPrimary = childIds[i % childIds.length];
      const teamSecond = i % 5 === 0 ? state.teams[rootNames[i % ROOTS]] : null;
      batch.push({
        externalRowId: username,
        username,
        displayName: `负载员工${seq}`,
        email: `${username}@loadgen.zhiyuan.test`,
        temporaryPassword: seedPassword,
        teamIds: [teamPrimary, ...(teamSecond ? [teamSecond] : [])],
        roleIds: [rolePrimary, ...(roleSecond ? [roleSecond] : [])],
        requirePasswordChange: false,
      });
      state.users[username] = true;
    }
    const started = Date.now();
    const result = await aep.call('POST', '/aep/v1/admin/users/import',
      {deploymentId: aep.deploymentId, users: batch}, {label: `seed:users-import(${batch.length})`});
    const seconds = (Date.now() - started) / 1000;
    console.log(`导入: created=${result.created} rejected=${result.rejected} — ${seconds.toFixed(1)}s（${(result.created / seconds).toFixed(1)} 用户/秒，Argon2 主导）`);
    if (result.errors?.length) console.log('样例错误:', JSON.stringify(result.errors.slice(0, 3)));
  } else {
    console.log('用户已达目标数量，跳过导入');
  }

  // ---- 4. data-scope rules ----
  const usernames = Object.keys(state.users);
  const sampleUser = (i) => `load-u${String((i * 37) % Math.max(1, usernames.length - 2) + 1).padStart(4, '0')}`;
  let ruleCount = state.ruleCount ?? 0;
  for (let i = ruleCount; i < RULES; i += 1) {
    const kinds = ['management_scope', 'exception_grant', 'exception_grant', 'management_scope', 'explicit_deny'];
    const subjects = [
      () => ({subjectType: 'user', subjectId: sampleUser(i)}),
      () => ({subjectType: 'role', subjectId: state.roles[roleNames[i % ROLES]]}),
      () => ({subjectType: 'team', subjectId: childIds[i % childIds.length]}),
    ];
    try {
      await aep.call('POST', '/aep/v1/admin/data-scope-rules', {
        ...(legacy ? {id: `load-rule-${String(i + 1).padStart(3, '0')}`} : {}),
        ruleKind: kinds[i % kinds.length],
        ...subjects[(i + Math.floor(i / 15)) % 3](),
        resourceKind: 'team',
        resourceId: childIds[(i * 7 + 3) % childIds.length],
        reason: `load-rule-${String(i + 1).padStart(3, '0')}`,
        ...(i % 4 === 0 ? {expiresAt: new Date(Date.now() + 7 * 86400e3).toISOString()} : {}),
      }, {label: 'seed:rule-create'});
    } catch (error) {
      if (!String(error.message).includes('DATA_SCOPE_RULE_EXISTS')) throw error;
    }
    ruleCount += 1;
  }
  state.ruleCount = ruleCount;
  console.log(`数据范围规则: ${ruleCount}`);

  // ---- 5. model assignments (catalog ids discovered live) ----
  const models = [];
  for await (const m of aep.walk('/aep/v1/admin/models', {label: 'seed:walk-models'})) models.push(m);
  const enabled = models.filter((m) => m.enabled !== false);
  console.log(`模型目录: ${models.map((m) => `${m.id}${m.enabled === false ? '(禁用)' : ''}`).join(', ')}`);
  if (!state.assignmentCount) {
    let n = 0;
    for (const teamId of childIds.slice(0, 10)) {
      await aep.call('POST', '/aep/v1/admin/model-assignments',
        {modelId: enabled[0]?.id, subject: {type: 'team', id: teamId}}, {label: 'seed:model-assign'});
      n += 1;
    }
    if (enabled[1]) {
      for (const rootName of rootNames) {
        await aep.call('POST', '/aep/v1/admin/model-assignments',
          {modelId: enabled[1].id, subject: {type: 'team', id: state.teams[rootName]}}, {label: 'seed:model-assign'});
        n += 1;
      }
    }
    for (let i = 0; i < 10; i += 1) {
      await aep.call('POST', '/aep/v1/admin/model-assignments',
        {modelId: enabled[enabled.length - 1]?.id, subject: {type: 'user', id: sampleUser(i + 5)}}, {label: 'seed:model-assign'});
      n += 1;
    }
    state.assignmentCount = n;
  }
  console.log(`模型分配: ${state.assignmentCount}`);

  // ---- 6. skills (registry entries only, no packages) ----
  if (!state.skillCount) {
    for (let i = 1; i <= 5; i += 1) {
      await aep.call('POST', '/aep/v1/admin/skills',
        {...(legacy ? {id: `load-skill-${i}`} : {}), name: `load-skill-${i}`, description: `负载测试技能 ${i}`, enabled: true}, {label: 'seed:skill-create'});
    }
    state.skillCount = 5;
  }

  // ---- 7. verify by walking ----
  const counts = {};
  for (const [name, path] of [['users', '/aep/v1/admin/users'], ['teams', '/aep/v1/admin/teams'],
    ['roles', '/aep/v1/admin/roles'], ['rules', '/aep/v1/admin/data-scope-rules'],
    ['models', '/aep/v1/admin/models']]) {
    counts[name] = await aep.count(path, `verify:walk-${name}`);
  }
  console.log('\n种子完成，全库计数:', JSON.stringify(counts));
  saveState(state);
  recorder.print('seed-org');
  recorder.save('seed-org');
}

main().catch((error) => {
  console.error('seed 失败:', error.message);
  saveState(state);
  process.exitCode = 1;
});
