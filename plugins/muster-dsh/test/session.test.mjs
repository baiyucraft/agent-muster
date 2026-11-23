import assert from 'node:assert/strict'
import { test } from 'node:test'
import { Hub } from '../dist/hub.js'
import { apply } from '../dist/index.js'
import { boundedReply, deliver, nativeOutcome } from '../dist/session.js'
import { bindTools, toolAllowed } from '../dist/tools.js'

test('completion sends metering only to a Hub that accepts it', async (t) => {
  const requests = []
  t.mock.method(globalThis, 'fetch', async (_url, options) => {
    requests.push(JSON.parse(options.body))
    return new Response(null, { status: 204 })
  })
  const hub = new Hub('http://127.0.0.1', 'test-token')
  const signal = new AbortController().signal
  const delivery = { id: 'message-test', lease: 'lease', kind: 'message' }
  const usage = { source: 'dsh_center_turn', input_tokens: 12, output_tokens: 5, total_tokens: 17 }
  const outcome = { text: '完成', status: 'handled', usage }
  await hub.complete(delivery, outcome, signal)
  await hub.complete({ ...delivery, usage_supported: true }, outcome, signal)
  assert.deepEqual(requests, [
    { lease: 'lease', text: '完成', status: 'handled' },
    { lease: 'lease', text: '完成', status: 'handled', usage },
  ])
})

const delivery = { id: 'test', attempt: 1, kind: 'message', text: '用户任务' }
const start = turn => ({ type: 'turn/start', data: { turn } })
const end = (turn, kind = 'completed') => ({ type: 'turn/end', data: { turn, reason: { kind } } })
const message = (id, kind = 'muster-dsh') => ({ type: 'user/message', data: { id, source: { kind } } })
const reply = text => ({ type: 'assistant/message', data: { message: { content: [{ type: 'text', text }] } } })
const native = events => ({ session: { snapshotEvents: () => events } })

function measuredReply(turn, step, text, usage) {
  return {
    type: 'assistant/message',
    data: {
      turn, step, usage, stream: [],
      message: {
        source: { provider: 'fixture', model: 'fixture-model' },
        content: [{ type: 'text', text }],
      },
    },
  }
}

const stepStart = (turn, step) => ({ type: 'step/start', data: { turn, step } })
const stepEnd = (turn, step) => ({ type: 'step/end', data: { turn, step } })

test('recovers the handled reply from the matching turn after a new lease', () => {
  const agent = native([
    start(1), message('muster:test:1'), reply('处理中'), reply('完成'), end(1),
    start(2), message('muster:other:1'), reply('另一个任务'), end(2),
  ])
  assert.deepEqual(nativeOutcome(agent, { ...delivery, attempt: 2 }), { text: '完成', status: 'handled' })
})

test('recovers the same measured turn under a new lease and excludes adjacent turns', () => {
  const usage = { inputTokens: 12, outputTokens: 3, cacheReadTokens: 5, cacheWriteTokens: 0, reasoningTokens: 2, totalTokens: 20 }
  const agent = native([
    start(1), message('muster:test:1'), stepStart(1, 1), measuredReply(1, 1, '进行中', usage), stepEnd(1, 1),
    stepStart(1, 2), measuredReply(1, 2, '完成', usage), stepEnd(1, 2), end(1),
    start(2), message('muster:other:1'), stepStart(2, 1), measuredReply(2, 1, '其他投递', usage), stepEnd(2, 1), end(2),
  ])
  const expected = {
    text: '完成',
    status: 'handled',
    usage: {
      source: 'dsh_center_turn',
      input_tokens: 24,
      output_tokens: 6,
      cached_input_tokens: 10,
      cache_write_input_tokens: 0,
      reasoning_output_tokens: 4,
      total_tokens: 40,
    },
  }
  assert.deepEqual(nativeOutcome(agent, delivery), expected)
  assert.deepEqual(nativeOutcome(agent, { ...delivery, attempt: 2 }), expected)
  assert.deepEqual(nativeOutcome(agent, { ...delivery, kind: 'report' }), expected)
})

test('omits unavailable buckets and ignores usage from interrupted or incomplete turns', () => {
  const usage = { inputTokens: 4, outputTokens: 2, cacheReadTokens: 3, totalTokens: 10 }
  const events = [
    start(1), message('muster:test:1'), stepStart(1, 1), measuredReply(1, 1, '完成', usage), stepEnd(1, 1),
  ]
  const completed = nativeOutcome(native([...events, end(1)]), delivery)
  assert.deepEqual(JSON.parse(JSON.stringify(completed)).usage, {
    source: 'dsh_center_turn',
    input_tokens: 4,
    output_tokens: 2,
    cached_input_tokens: 3,
    total_tokens: 10,
  })
  const interrupted = nativeOutcome(native([...events, end(1, 'aborted')]), delivery)
  assert.equal(interrupted.status, 'interrupted')
  assert.equal(interrupted.usage, undefined)
  const incomplete = nativeOutcome(native([
    ...events, stepStart(1, 2), measuredReply(1, 2, '完成', undefined), stepEnd(1, 2), end(1),
  ]), delivery)
  assert.deepEqual(incomplete, { text: '完成', status: 'handled' })
})

test('includes a measured retry attempt and keeps historical replies with unreadable usage', () => {
  const usage = { inputTokens: 3, outputTokens: 1, totalTokens: 4 }
  const prefix = [start(1), message('muster:test:1'), stepStart(1, 1)]
  const events = [
    ...prefix,
    {
      type: 'assistant/attempt',
      data: { turn: 1, step: 1, stream: [{ type: 'chunk', time: 0, chunk: { type: 'usage', usage } }] },
    },
    { type: 'llm/retry', data: { turn: 1, step: 1 } },
    { type: 'llm/retry-started', data: { turn: 1, step: 1 } },
    measuredReply(1, 1, '完成', usage), stepEnd(1, 1), end(1),
  ]
  assert.equal(nativeOutcome(native(events), delivery).usage.total_tokens, 8)

  const legacy = native([...prefix, reply('已有回复'), stepEnd(1, 1), end(1)])
  assert.deepEqual(nativeOutcome(legacy, delivery), { text: '已有回复', status: 'handled' })
})

test('a claimed but interrupted user turn is never replayed as new work', () => {
  const agent = {
    ...native([start(1), message('muster:test:1'), reply('草稿')]),
    inbox: { nextTurn: [], splice() {} },
  }
  assert.equal(nativeOutcome(agent, delivery).status, 'interrupted')
  assert.equal(nativeOutcome(agent, { ...delivery, attempt: 2 }), undefined)
  let continuation
  deliver({ ...agent, followup: item => { continuation = item } }, { ...delivery, attempt: 2 })
  assert.equal(continuation.id, 'muster:test:2')
  assert.match(continuation.content[0].text, /最后一条尚未完成/)
})

test('a user follow-up carries the selected task ID before and after a retry', () => {
  const first = { ...delivery, task_id: 'task-selected', text: '读下一行' }
  for (const [attempt, events] of [[1, []], [2, [start(1), message('muster:test:1'), end(1, 'aborted')]]]) {
    let input
    deliver({
      ...native(events), inbox: { nextTurn: [] }, followup: item => { input = item },
    }, { ...first, attempt })
    assert.match(input.content[0].text, /"task_id":"task-selected","text":"读下一行"/)
  }
})

test('the center exposes only its registered Muster tools to execution', () => {
  for (const name of ['muster_list_projects', 'muster_dispatch', 'muster_dispatch_plan', 'muster_resume_task', 'muster_get_task',
    'muster_binding_candidates', 'muster_propose_binding']) {
    assert.equal(toolAllowed(name), true)
  }
  assert.equal(toolAllowed('subagent'), false)
  assert.equal(toolAllowed('bash'), false)
})

function toolAgent() {
  const tools = new Map()
  const agent = { ctx: { tools: {
    register(tool) { tools.set(tool.name, tool); return () => tools.delete(tool.name) },
    restrict: () => () => {},
    guard: () => () => {},
  } } }
  return { tools, agent }
}

test('dispatch only opts in to Git publication with the dedicated workflow', async () => {
  const { tools, agent } = toolAgent()
  const signal = new AbortController().signal
  let current = delivery
  const hub = { dispatch: async (_delivery, input) => JSON.parse(JSON.stringify(input)) }
  const dispose = bindTools(agent, hub, () => ({ delivery: current, signal }))
  try {
    const input = { project_id: 'project-1', title: '修复问题', prompt: '运行测试' }
    const execute = tools.get('muster_dispatch').execute
    assert.deepEqual(await execute(input, { agent, signal }), input)
    assert.deepEqual(await execute({ ...input, workflow: 'git_verify' }, { agent, signal }), { ...input, git_delivery: true })
    assert.deepEqual(await execute({ ...input, workflow: 'git_publish' }, { agent, signal }), { ...input, git_delivery: true, git_publish: true })
    await assert.rejects(() => execute({ ...input, workflow: 'unknown' }, { agent, signal }), /未知的项目工作流/)
    current = { ...delivery, task_id: 'existing-task' }
    await assert.rejects(() => execute({ ...input, workflow: 'git_publish' }, { agent, signal }), /只能在创建新任务时启用/)
  } finally {
    dispose()
  }
})

test('group dispatch submits ordered steps once and rejects unsupported deliveries or invalid dependencies', async (t) => {
  const { tools, agent } = toolAgent()
  const signal = new AbortController().signal
  const requests = []
  t.mock.method(globalThis, 'fetch', async (url, options) => {
    requests.push({ path: url.pathname, input: JSON.parse(options.body) })
    return Response.json({ task_id: 'task-root' })
  })
  const hub = new Hub('http://127.0.0.1', 'test-token')
  const compatible = { ...delivery, id: 'message-id', lease: 'lease', plan_supported: true }
  let current = compatible
  const dispose = bindTools(agent, hub, () => ({ delivery: current, signal }))
  const title = '整理素材'
  const steps = [
    { project_id: 'materials', title: '建立索引', prompt: '整理素材并给出索引。' },
    { project_id: 'docs', binding_id: 'home', title: '撰写说明', prompt: '根据索引写一份说明。', depends_on: [1] },
  ]
  const input = { title, steps }
  try {
    const execute = tools.get('muster_dispatch_plan').execute
    assert.deepEqual(await execute(input, { agent, signal }), { task_id: 'task-root' })
    await assert.rejects(() => execute({ title, steps: steps.slice(0, 1) }, { agent, signal }), /2 至 8/)
    for (const depends_on of [[0], [2], [1, 1], ['1']]) {
      const malformed = [steps[0], { ...steps[1], depends_on }]
      await assert.rejects(() => execute({ title, steps: malformed }, { agent, signal }), /前序步骤编号/)
    }
    current = { ...compatible, plan_supported: undefined }
    await assert.rejects(() => execute(input, { agent, signal }), /请使用 muster_dispatch/)
    current = { ...compatible, kind: 'report' }
    await assert.rejects(() => execute(input, { agent, signal }), /只允许查询/)
    current = { ...compatible, task_id: 'task-existing' }
    await assert.rejects(() => execute(input, { agent, signal }), /请使用 muster_resume_task/)
    assert.deepEqual(requests, [{ path: '/api/v1/plugin/deliveries/message-id/dispatch', input: { lease: 'lease', ...input } }])
  } finally {
    dispose()
  }
})

test('the native input advertises group dispatch only for a supported new user request', () => {
  function inputText(delivery) {
    let input
    deliver({
      ...native([]), inbox: { nextTurn: [] }, followup: item => { input = item },
    }, delivery)
    return input.content[0].text
  }
  const compatible = { ...delivery, plan_supported: true }
  assert.equal(inputText(delivery), delivery.text)
  assert.match(inputText(compatible), /支持分组派发/)
  assert.doesNotMatch(inputText({ ...compatible, kind: 'report' }), /支持分组派发/)
  assert.doesNotMatch(inputText({ ...compatible, task_id: 'task-existing' }), /支持分组派发/)
  const proposals = { ...compatible, binding_proposals_supported: true }
  assert.match(inputText(proposals), /支持项目绑定提案/)
  assert.doesNotMatch(inputText(compatible), /支持项目绑定提案/)
  assert.doesNotMatch(inputText({ ...proposals, kind: 'report' }), /支持项目绑定提案/)
  assert.doesNotMatch(inputText({ ...proposals, task_id: 'task-existing' }), /支持项目绑定提案/)
})

const bindingFixture = {
  name: '素材',
  bindings: [{ device_id: 'device-a', directory_id: 'dir-a', agent_kind: 'dsh', is_default: true }],
}
const proposalFixture = {
  ...bindingFixture, id: 'proposal-id', message_id: 'message-id', status: 'pending', created_at: '2026-11-23T00:00:00Z',
}

test('queries registered candidates and submits a stable leased proposal for a supported new message', async (t) => {
  const { tools, agent } = toolAgent()
  const signal = new AbortController().signal
  const requests = []
  const candidates = {
    candidates: [{
      device_id: 'device-a', device_name: '家里电脑', directories: [{ id: 'dir-a', label: '素材' }],
      agents: [{ id: 'dsh', version: '0.2.1-rc.5', status: 'supported',
        capabilities: { prompt: true, final_reply: true, resume: true } }],
    }],
  }
  t.mock.method(globalThis, 'fetch', async (url, options) => {
    requests.push({ method: options.method, path: url.pathname, input: options.body && JSON.parse(options.body) })
    return Response.json(options.method === 'GET' ? candidates : proposalFixture)
  })
  const hub = new Hub('http://127.0.0.1', 'test-token')
  const supported = { ...delivery, id: 'message-id', lease: 'lease', binding_proposals_supported: true }
  let current = supported
  const dispose = bindTools(agent, hub, () => ({ delivery: current, signal }))
  const query = tools.get('muster_binding_candidates').execute
  const propose = tools.get('muster_propose_binding').execute
  try {
    assert.deepEqual(await query({}, { agent, signal }), candidates)
    assert.deepEqual(await propose({ ...bindingFixture, name: ' 素材 ' }, { agent, signal }), proposalFixture)
    assert.deepEqual(current.binding_proposal, proposalFixture)
    assert.deepEqual(await propose(bindingFixture, { agent, signal }), proposalFixture)
    await assert.rejects(() => tools.get('muster_dispatch').execute(
      { project_id: 'project', title: '派发', prompt: '执行' }, { agent, signal },
    ), /已为本条消息提交项目绑定提案/)

    for (const bindings of [[], [{ ...bindingFixture.bindings[0], is_default: false }], [
      { ...bindingFixture.bindings[0], agent_kind: 'unknown' },
    ]]) {
      await assert.rejects(() => propose({ ...bindingFixture, bindings }, { agent, signal }), /绑定/)
    }
    for (current of [delivery, { ...supported, kind: 'report' }, { ...supported, task_id: 'task-existing' }]) {
      await assert.rejects(() => query({}, { agent, signal }), /未启用项目绑定提案/)
      await assert.rejects(() => propose(bindingFixture, { agent, signal }), /未启用项目绑定提案/)
    }
    const expectedPost = {
      method: 'POST', path: '/api/v1/plugin/deliveries/message-id/binding-proposal',
      input: { lease: 'lease', ...bindingFixture },
    }
    assert.deepEqual(requests, [
      { method: 'GET', path: '/api/v1/plugin/binding-candidates', input: undefined },
      expectedPost, expectedPost,
    ])
  } finally {
    dispose()
  }
})

test('recovers a committed proposal after lease loss without rerunning the interrupted model turn', () => {
  const incomplete = native([start(1), message('muster:test:1'), reply('草稿'), end(1, 'aborted')])
  const reclaimed = { ...delivery, attempt: 2, binding_proposal: proposalFixture }
  assert.deepEqual(nativeOutcome(incomplete, reclaimed), { text: '', status: 'handled' })
  assert.deepEqual(nativeOutcome(incomplete, {
    ...reclaimed, binding_proposal: { ...proposalFixture, status: 'accepted', project_id: 'project-a' },
  }), { text: '', status: 'handled' })
  assert.deepEqual(nativeOutcome(incomplete, {
    ...reclaimed, binding_proposal: { ...proposalFixture, status: 'rejected' },
  }), { text: '', status: 'handled' })
  const complete = native([start(1), message('muster:test:1'), reply('请确认素材项目'), end(1)])
  assert.deepEqual(nativeOutcome(complete, reclaimed), { text: '', status: 'handled' })
  assert.equal(nativeOutcome(incomplete, { ...delivery, attempt: 2 }), undefined)
})

test('a reclaimed proposal completes even when the native session cannot be resolved', async (t) => {
  const address = 'MUSTER_DSH_HUB_URL'
  const token = 'MUSTER_DSH_PLUGIN_TOKEN'
  for (const key of [address, token]) {
    const original = process.env[key]
    t.after(() => {
      if (original === undefined) delete process.env[key]
      else process.env[key] = original
    })
  }
  process.env[address] = 'http://127.0.0.1'
  process.env[token] = 'test-token'
  let complete
  const completion = new Promise(resolve => { complete = resolve })
  let claimed = false
  t.mock.method(globalThis, 'fetch', async (url, options) => {
    if (String(url).endsWith('/claim') && !claimed) {
      claimed = true
      return Response.json({ ...delivery, id: 'message-test', lease: 'current-lease', binding_proposal: proposalFixture })
    }
    if (String(url).endsWith('/complete')) complete(JSON.parse(options.body))
    return new Response(null, { status: 204 })
  })
  let sessionOpened = false
  let stop
  await apply({
    logger: { info() {}, warn() {} },
    sessionController: { resolveAgent() {
      sessionOpened = true
      throw new Error('session unavailable')
    } },
    effect(callback) { stop = callback() },
  })
  let timer
  try {
    const result = await Promise.race([completion, new Promise((_resolve, reject) => {
      timer = setTimeout(() => reject(new Error('delivery did not complete')), 1_000)
    })])
    assert.deepEqual(result, { lease: 'current-lease', text: '', status: 'handled' })
    assert.equal(sessionOpened, false)
  } finally {
    clearTimeout(timer)
    await stop()
  }
})

test('an interrupted report can be generated after re-claim', () => {
  const agent = native([start(1), message('muster:test:1'), reply('草稿'), end(1, 'aborted')])
  assert.equal(nativeOutcome(agent, { ...delivery, kind: 'report' }).status, 'interrupted')
  assert.equal(nativeOutcome(agent, { ...delivery, kind: 'report', attempt: 2 }), undefined)
})

test('parked native input retains its original ID when re-claimed', () => {
  const pending = { id: 'muster:test:1', source: { kind: 'muster-dsh' } }
  const queue = [pending]
  const calls = []
  deliver({
    inbox: { nextTurn: queue, splice: (_, start, count) => queue.splice(start, count) },
    followup: item => calls.push(item),
  }, { ...delivery, attempt: 2 })
  assert.deepEqual(queue, [])
  assert.deepEqual(calls, [pending])
})

test('reply size is bounded at a UTF-8 character boundary', () => {
  const reply = boundedReply('中😀'.repeat(3000))
  assert.ok(Buffer.byteLength(reply) <= 12_000)
  assert.ok(reply.endsWith('（回复已截断。）'))
  assert.ok(!reply.includes('\ufffd'))
})
