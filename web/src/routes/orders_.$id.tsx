import { createFileRoute, Link, redirect } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { useState } from 'react'
import { z } from 'zod'
import { client } from '../api/client'
import { Button } from '../components/ui/button'
import { Card } from '../components/ui/card'
import { apiRequestError } from '../lib/api-error'
import { orderStatusLabel, refundStatusLabel } from '../features/order/order-status'
import { useDecideRefund, useRefundQuery, useRequestRefund } from '../features/order/refund'

export const Route = createFileRoute('/orders_/$id')({
  beforeLoad: ({ context }) => {
    if (!context.auth.getState().accessToken) throw redirect({ to: '/login' })
  },
  validateSearch: z.object({ merchant: z.boolean().optional() }),
  component: OrderDetailPage,
})

function OrderDetailPage() {
  const { id } = Route.useParams()
  const { merchant = false } = Route.useSearch()
  const orderId = Number(id)
  const [reason, setReason] = useState('')
  const [showRefundForm, setShowRefundForm] = useState(false)
  const order = useQuery({
    queryKey: ['order', merchant, id],
    queryFn: async () => {
      const params = { path: { id: orderId } }
      const res = merchant
        ? await client.GET('/api/v1/merchant/orders/{id}', { params })
        : await client.GET('/api/v1/orders/{id}', { params })
      if (res.error) throw apiRequestError(res)
      return res.data
    },
  })
  // 404 REFUND_NOT_FOUND 已在 hook 内翻译成 null（「还没有退款单」不是错误）
  const refund = useRefundQuery(orderId, merchant)
  const requestRefund = useRequestRefund()
  const decideRefund = useDecideRefund()

  const canApply = !merchant && (order.data?.status === 'PAID' || order.data?.status === 'FULFILLING')
  const canDecide = merchant && order.data?.status === 'REFUND_REQUESTED'

  return (
    <div className="mx-auto max-w-2xl px-4 py-8">
      <Link to="/orders" className="text-sm text-amber-300 hover:underline">← 返回订单列表</Link>
      {order.isPending && <p className="mt-6 text-zinc-400">订单加载中…</p>}
      {order.error && <p className="mt-6 text-red-400">{order.error.message}</p>}
      {order.data && <Card className="mt-5 space-y-3 p-5">
        <h1 className="text-xl font-semibold">订单详情</h1>
        <p className="break-all text-sm text-zinc-500">{order.data.order_no}</p>
        <p>状态：{orderStatusLabel(order.data.status)}</p>
        <p>商品：{order.data.item.product_title}</p>
        <p>规格：{order.data.item.sku_spec || '默认规格'}</p>
        <p>数量：{order.data.item.quantity}</p>
        <p>单价：¥{(order.data.item.unit_price / 100).toFixed(2)}</p>
        <p className="font-semibold">合计：¥{(order.data.total_amount / 100).toFixed(2)}</p>
        <div className="border-t border-zinc-800 pt-3 text-sm text-zinc-400">
          <p>创建：{new Date(order.data.created_at).toLocaleString()}</p>
          {order.data.paid_at && <p>支付：{new Date(order.data.paid_at).toLocaleString()}</p>}
          {order.data.fulfilling_at && <p>发货：{new Date(order.data.fulfilling_at).toLocaleString()}</p>}
          {order.data.auto_complete_at && <p className="text-amber-300">将于 {new Date(order.data.auto_complete_at).toLocaleString()} 自动确认收货</p>}
          {order.data.completed_at && <p>完成：{new Date(order.data.completed_at).toLocaleString()}</p>}
          {order.data.canceled_at && <p>取消：{new Date(order.data.canceled_at).toLocaleString()}</p>}
          {order.data.expired_at && <p>超时：{new Date(order.data.expired_at).toLocaleString()}</p>}
        </div>

        {/* 售后区：退款单是订单的从事实体，没有则展示可申请入口 */}
        {refund.data && (
          <div className="space-y-1 border-t border-zinc-800 pt-3 text-sm">
            <p className="font-medium">售后 · {refundStatusLabel(refund.data.status)}</p>
            <p className="break-all text-xs text-zinc-500">退款单号 {refund.data.refund_no}</p>
            <p>退款金额：¥{(refund.data.amount / 100).toFixed(2)}</p>
            <p>申请原因：{refund.data.reason}</p>
            <p className="text-zinc-400">申请时间：{new Date(refund.data.requested_at).toLocaleString()}</p>
            {refund.data.decided_at && <p className="text-zinc-400">裁决时间：{new Date(refund.data.decided_at).toLocaleString()}</p>}
            {refund.data.decision_remark && <p className="text-zinc-400">商家备注：{refund.data.decision_remark}</p>}
            {refund.data.refunded_at && <p className="text-zinc-400">退款时间：{new Date(refund.data.refunded_at).toLocaleString()}</p>}
            {refund.data.status === 'SUCCEEDED' && <p className="text-zinc-400">{refund.data.restore_stock ? '已回补库存' : '已发货订单不回补库存'}</p>}
          </div>
        )}

        {canApply && (
          <div className="flex flex-wrap items-center gap-2 border-t border-zinc-800 pt-3">
            {showRefundForm ? (
              <>
                <input
                  value={reason}
                  onChange={(e) => setReason(e.target.value)}
                  placeholder="退款原因"
                  maxLength={200}
                  className="w-48 rounded-lg border border-zinc-700 bg-zinc-900 px-2 py-1 text-sm"
                />
                <Button
                  size="sm"
                  disabled={!reason.trim() || requestRefund.isPending}
                  onClick={() =>
                    requestRefund.mutate(
                      { orderId, reason: reason.trim() },
                      { onSuccess: () => { setShowRefundForm(false); setReason('') } },
                    )
                  }
                >
                  提交申请
                </Button>
                <Button size="sm" variant="outline" onClick={() => setShowRefundForm(false)}>取消</Button>
              </>
            ) : (
              <Button size="sm" variant="outline" onClick={() => setShowRefundForm(true)}>申请退款</Button>
            )}
          </div>
        )}

        {canDecide && (
          <div className="flex gap-2 border-t border-zinc-800 pt-3">
            <Button size="sm" disabled={decideRefund.isPending} onClick={() => decideRefund.mutate({ orderId, approve: true })}>
              同意退款
            </Button>
            <Button size="sm" variant="outline" disabled={decideRefund.isPending} onClick={() => decideRefund.mutate({ orderId, approve: false })}>
              拒绝退款
            </Button>
          </div>
        )}
      </Card>}
    </div>
  )
}
