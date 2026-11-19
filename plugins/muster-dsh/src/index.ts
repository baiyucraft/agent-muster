import type { Context } from '@deepseek-ai/cordis'

export const name = 'muster-dsh'

export function apply(ctx: Context): void {
  ctx.logger.info('Muster dsh plugin loaded')
}
