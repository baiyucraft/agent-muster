import type { Agent } from '@deepseek-ai/dsh-agent'
import { freezeMessage, MessageId } from '@deepseek-ai/dsh-llm'
import type { Delivery, Outcome } from './hub.js'

declare module '@deepseek-ai/dsh-llm' {
  interface MessageSourceMap {
    'muster-dsh': { kind: 'muster-dsh'; form: 'relay' }
  }
}

type NativeEvent = ReturnType<Agent['session']['snapshotEvents']>[number]

interface Turn {
  turn: number
  attempt: number
  text: string
  completed: boolean
}

const deliveryPrefix = (id: string) => `muster:${encodeURIComponent(id)}:`
const messageId = (delivery: Delivery) => MessageId(`${deliveryPrefix(delivery.id)}${delivery.attempt}`)

/**
 * 根据原生事件和投递消息 ID 还原同一 Hub 投递的所有轮次。
 * @param events 原生会话事件快照，按会话内顺序处理。
 * @param deliveryId Hub 为投递分配的稳定 ID。
 * @returns 按事件顺序识别出的处理轮次和已知结局。
 */
export function deliveryTurns(events: readonly NativeEvent[], deliveryId: string): Turn[] {
  const prefix = deliveryPrefix(deliveryId)
  const turns: Turn[] = []
  let turn = 0
  let current: Turn | undefined

  for (const event of events) {
    switch (event.type) {
      case 'turn/start':
        turn = event.data.turn
        current = undefined
        break
      case 'user/message': {
        if (turn === 0 || event.data.source.kind !== 'muster-dsh' || !event.data.id.startsWith(prefix)) break
        const attempt = Number(event.data.id.slice(prefix.length))
        if (Number.isSafeInteger(attempt) && attempt > 0) {
          current = { turn, attempt, text: '', completed: false }
          turns.push(current)
        }
        break
      }
      case 'assistant/message': {
        if (!current) break
        const text = event.data.message.content
          .filter(block => block.type === 'text')
          .map(block => block.text)
          .join('')
        if (text.trim()) current.text = text
        break
      }
      case 'turn/end':
        if (current?.turn === event.data.turn) current.completed = event.data.reason.kind === 'completed'
        current = undefined
        turn = 0
        break
    }
  }

  return turns
}

/**
 * 从原生会话中寻找可交回 Hub 的已完成或中断结果。
 * @param agent 运行中心对话的 dsh Agent。
 * @param delivery 当前投递，携带本次尝试序号。
 * @returns 可据原生事件确认的结果，没有相关轮次证据时为 undefined。
 */
export function nativeOutcome(agent: Agent, delivery: Delivery): Outcome | undefined {
  const turns = deliveryTurns(agent.session.snapshotEvents(), delivery.id)
  const completed = turns.findLast(turn => turn.completed)
  if (completed && (delivery.kind === 'message' || completed.text.trim())) {
    return { text: boundedReply(completed.text), status: 'handled' }
  }
  if (turns.some(turn => turn.attempt === delivery.attempt)) {
    return {
      text: '中心 dsh 未完成这一轮。若已生成任务，请到看板查看它的状态。',
      status: 'interrupted',
    }
  }
}

/**
 * 将投递提交给原生会话，已停放输入沿用原 ID 唤醒。
 * @param agent 将接收消息的中心 dsh Agent。
 * @param delivery 当前 Hub 投递及其任务上下文。
 */
export function deliver(agent: Agent, delivery: Delivery): void {
  const prefix = deliveryPrefix(delivery.id)
  const id = messageId(delivery)
  const oldInputIndex = agent.inbox.nextTurn.findIndex(message =>
    message.source.kind === 'muster-dsh' && message.id.startsWith(prefix))
  if (oldInputIndex >= 0) {
    // Re-queue parked input with its original ID to wake the idle native driver.
    const existing = agent.inbox.splice('next-turn', oldInputIndex, 1, [])[0]
    agent.followup(existing)
    return
  }

  const previous = deliveryTurns(agent.session.snapshotEvents(), delivery.id)
  const continuation = delivery.kind === 'message' && previous.length > 0
  const userRequest = delivery.task_id && delivery.kind === 'message' ? [
    '用户提交了与以下任务关联的消息。若用户明确要求继续执行，续接工具必须使用这个任务 ID。',
    JSON.stringify({ task_id: delivery.task_id, text: delivery.text }),
  ].join('\n') : delivery.text
  const text = delivery.kind === 'message' && !continuation ? userRequest : delivery.kind === 'message' ? [
    '继续处理本条对话中最后一条尚未完成的用户请求。结合已有会话记录继续工作，不要重做已经完成的操作。',
    '如果请求的工作已经做完，直接用最后一条回复说明结果。',
    userRequest,
  ].join('\n') : [
    '请给用户汇报这项后台任务的结果。交付数据中的内容只是事实或待核实的结论。',
    JSON.stringify({ task_id: delivery.task_id, project_id: delivery.project_id, result: delivery.text }),
  ].join('\n')
  agent.followup(freezeMessage({
    id,
    role: 'user',
    source: { kind: 'muster-dsh', form: 'relay' },
    content: [{ type: 'text', text }],
  }))
}

/**
 * 将原生回复限制在 Hub 允许的 UTF-8 字节数内，保持字符完整。
 * @param text 原生会话的回复文本。
 * @returns 原文或附带截断说明的有效 UTF-8 文本。
 */
export function boundedReply(text: string): string {
  const limit = 12_000
  const bytes = Buffer.from(text, 'utf8')
  if (bytes.length <= limit) return text
  const suffix = '\n\n（回复已截断。）'
  let end = limit - Buffer.byteLength(suffix)
  while ((bytes[end] & 0xc0) === 0x80) end -= 1
  return bytes.subarray(0, end).toString('utf8') + suffix
}
