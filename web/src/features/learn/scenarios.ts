import type { ChapterId } from './chapters'
import type { Scenario } from './scenario'
import { assemblyScenarios, authScenarios, uploadScenarios } from './scenarios-core'
import { commerceScenarios, orderScenarios, raceScenarios, refundScenarios } from './scenarios-trade'
import { eventScenarios, feedScenarios, pageScenarios } from './scenarios-read'

export const scenariosByChapter: Record<ChapterId, Scenario[]> = {
  assembly: assemblyScenarios,
  auth: authScenarios,
  upload: uploadScenarios,
  commerce: commerceScenarios,
  order: orderScenarios,
  refund: refundScenarios,
  races: raceScenarios,
  feed: feedScenarios,
  events: eventScenarios,
  pages: pageScenarios,
}
