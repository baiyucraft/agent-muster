import assert from 'node:assert/strict'
import { test } from 'node:test'
import { boundedReply, deliver, nativeOutcome } from '../dist/session.js'
import { bindTools, toolAllowed } from '../dist/tools.js'

const delivery = { id: 'test', attempt: 1, kind: 'message', text: '用户任务' }
const start = turn => ({ type: 'turn/start', data: { turn } })
const end = (turn, kind = 'completed') => ({ type: 'turn/end', data: { turn, reason: { kind } } })
const message = (id, kind = 'muster-dsh') => ({ type: 'user/message', data: { id, source: { kind } } })
const reply = text => ({ type: 'assistant/message', data: { message: { content: [{ type: 'text', text }] } } })
const native = events => ({ session: { snapshotEvents: () => events } })

test('recovers the handled reply from the matching turn after a new lease', () => {
  const agent = native([
    start(1), message('muster:test:1'), reply('处理中'), reply('完成'), end(1),
    start(2), message('muster:other:1'), reply('另一个任务'), end(2),
  ])
  assert.deepEqual(nativeOutcome(agent, { ...delivery, attempt: 2 }), { text: '完成', status: 'handled' })
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

test('the center exposes only its four registered Muster tools to execution', () => {
  for (const name of ['muster_list_projects', 'muster_dispatch', 'muster_resume_task', 'muster_get_task']) {
    assert.equal(toolAllowed(name), true)
  }
  assert.equal(toolAllowed('subagent'), false)
  assert.equal(toolAllowed('bash'), false)
})

test('dispatch only opts in to Git publication with the dedicated workflow', async () => {
  const tools = new Map()
  const agent = { ctx: { tools: {
    register(tool) { tools.set(tool.name, tool); return () => tools.delete(tool.name) },
    restrict: () => () => {},
    guard: () => () => {},
  } } }
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
