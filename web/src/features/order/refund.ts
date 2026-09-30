import { useMutation, useQuery, useQueryClient, type QueryClient } from '@tanstack/react-query'
import { toast } from 'sonner'
import { client } from '../../api/client'
import { apiRequestError } from '../../lib/api-error'
import type { components } from '../../api/types'

export type RefundDetail = components['schemas']['RefundDetail']

/**
 * 退款会同时改变「订单列表」「订单详情」「作者成交统计」（以及退款单本身），
 * 统一在这里失效，避免某个入口漏掉导致 UI 停在旧状态。
 */
export function invalidateOrderCaches(qc: QueryClient) {
  qc.invalidateQueries({ queryKey: ['orders'] })
  // 前缀匹配 ['order', merchant, id]：买/商家两个详情缓存都失效
  qc.invalidateQueries({ queryKey: ['order'] })
  qc.invalidateQueries({ queryKey: ['refund'] })
  qc.invalidateQueries({ queryKey: ['author-sales'] })
}

/**
 * 查最新一笔退款单。
 * 404 + REFUND_NOT_FOUND 是「这笔订单还没有退款」的正常语义，返回 null 而不是抛错——
 * 否则订单详情页会把「没退款」渲染成红色错误。
 */
export function useRefundQuery(orderId: number, merchant: boolean) {
  return useQuery({
    queryKey: ['refund', merchant ? 'merchant' : 'buyer', String(orderId)],
    queryFn: async (): Promise<RefundDetail | null> => {
      const params = { path: { id: orderId } }
      const res = merchant
        ? await client.GET('/api/v1/merchant/orders/{id}/refund', { params })
        : await client.GET('/api/v1/orders/{id}/refund', { params })
      if (res.error) {
        if (res.response.status === 404 && res.error.error.code === 'REFUND_NOT_FOUND') return null
        throw apiRequestError(res)
      }
      return res.data
    },
  })
}

/** 买家申请退款（PAID / FULFILLING 才允许，后端状态机兜底） */
export function useRequestRefund() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: async ({ orderId, reason }: { orderId: number; reason: string }) => {
      const res = await client.POST('/api/v1/orders/{id}/refund', {
        params: { path: { id: orderId } },
        body: { reason },
      })
      if (res.error) throw apiRequestError(res)
      return res.data
    },
    onSuccess: (data) => {
      // replayed=true：同一售后事实的幂等重放，不是新申请
      toast.success(data?.replayed ? '该订单已有退款申请' : '退款申请已提交')
      invalidateOrderCaches(qc)
    },
    onError: (error) => toast.error(error.message),
  })
}

/** 商家裁决退款：approve=true 同意（订单终态 REFUNDED）/ false 拒绝（订单退回申请前状态） */
export function useDecideRefund() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: async ({
      orderId,
      approve,
      remark,
    }: {
      orderId: number
      approve: boolean
      remark?: string
    }) => {
      const params = { path: { id: orderId } }
      const body = remark ? { remark } : {}
      const res = approve
        ? await client.POST('/api/v1/merchant/orders/{id}/refund/approve', { params, body })
        : await client.POST('/api/v1/merchant/orders/{id}/refund/reject', { params, body })
      if (res.error) throw apiRequestError(res)
      return res.data
    },
    onSuccess: (data, input) => {
      if (!input.approve) {
        toast.success('已拒绝退款')
      } else {
        toast.success(data?.restore_stock ? '已同意退款，库存已回补' : '已同意退款')
      }
      invalidateOrderCaches(qc)
    },
    onError: (error) => toast.error(error.message),
  })
}
