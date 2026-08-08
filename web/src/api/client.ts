import createClient from 'openapi-fetch'
import type { paths } from './types'

/**
 * 契约优先：client 类型完全来自 openapi.yaml 生成的 paths。
 * 生产环境通过 VITE_API_BASE_URL 指向真实后端（同源部署时留空）。
 */
export const API_BASE_URL = import.meta.env.VITE_API_BASE_URL ?? ''

export const client = createClient<paths>({
  baseUrl: API_BASE_URL,
})
