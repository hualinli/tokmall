export type Scenario = {
  id: string
  title: string
  foundation: string
  apply: string
  code: { file: string; lines: string; body: string }
}
