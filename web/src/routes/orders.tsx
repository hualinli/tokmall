import { createFileRoute, Link, redirect } from '@tanstack/react-router'
import { useInfiniteQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { toast } from 'sonner'
import { client } from '../api/client'
import type { components } from '../api/types'
import { Button } from '../components/ui/button'
import { Card } from '../components/ui/card'
import { apiErrorMessage } from '../lib/api-error'

type Order = components['schemas']['OrderDetail']

export const Route = createFileRoute('/orders')({
  beforeLoad: ({ context }) => {
    if (!context.auth.getState().accessToken) throw redirect({ to: '/login' })
  },
  component: OrdersPage,
})

const labels: Record<Order['status'], string> = {
  PENDING_PAYMENT: '待支付', PAID: '待发货', FULFILLING: '待确认收货',
  COMPLETED: '已完成', CANCELED: '已取消', EXPIRED: '已超时',
}

function OrdersPage() {
  const [merchant, setMerchant] = useState(false)
  const queryClient = useQueryClient()
  const key = ['orders', merchant ? 'merchant' : 'buyer']
  const orders = useInfiniteQuery({
    queryKey: key,
    initialPageParam: undefined as number | undefined,
    queryFn: async ({ pageParam }) => {
      const params = { query: { limit: 20, before_id: pageParam } }
      const res = merchant
        ? await client.GET('/api/v1/merchant/orders', { params })
        : await client.GET('/api/v1/orders', { params })
      if (res.error) throw new Error(apiErrorMessage(res.error))
      return res.data
    },
    getNextPageParam: (last) => last.next_before_id,
  })
  const action = useMutation({
    mutationFn: async ({ order, kind }: { order: Order; kind: 'pay' | 'cancel' | 'fulfill' | 'complete' }) => {
      const path = { id: order.order_id }
      const res = kind === 'pay'
        ? await client.POST('/api/v1/orders/{id}/pay', {
            params: { path },
            body: { payment_no: `sim-${order.order_no}`, provider_trade_no: `sim-${order.order_no}`, amount: order.total_amount },
          })
        : kind === 'cancel'
          ? await client.POST('/api/v1/orders/{id}/cancel', { params: { path } })
          : kind === 'fulfill'
            ? await client.POST('/api/v1/merchant/orders/{id}/fulfill', { params: { path } })
            : await client.POST('/api/v1/orders/{id}/complete', { params: { path } })
      if (res.error) throw new Error(apiErrorMessage(res.error))
    },
    onSuccess: () => {
      toast.success('订单状态已更新')
      queryClient.invalidateQueries({ queryKey: ['orders'] })
      queryClient.invalidateQueries({ queryKey: ['author-sales'] })
    },
    onError: (error) => toast.error(error.message),
  })
  const items = orders.data?.pages.flatMap((page) => page.items) ?? []

  return (
    <div className="mx-auto max-w-3xl px-4 py-8">
      <h1 className="text-2xl font-semibold">订单</h1>
      <p className="mt-2 text-sm text-zinc-500">支付和发货为学习环境中的模拟操作。</p>
      <div className="mt-5 flex gap-2">
        <Button variant={merchant ? 'outline' : 'default'} onClick={() => setMerchant(false)}>我购买的</Button>
        <Button variant={merchant ? 'default' : 'outline'} onClick={() => setMerchant(true)}>我销售的</Button>
      </div>
      {orders.isPending && <p className="mt-6 text-zinc-400">订单加载中…</p>}
      {orders.error && <p className="mt-6 text-red-400">{orders.error.message}</p>}
      {!orders.isPending && !orders.error && items.length === 0 && <p className="mt-6 text-zinc-400">暂无订单</p>}
      <div className="mt-5 space-y-3">
        {items.map((order) => (
          <Card key={order.order_id} className="p-4">
            <div className="flex flex-wrap items-start justify-between gap-2">
              <div>
                <Link to="/orders/$id" params={{ id: String(order.order_id) }} search={{ merchant }} className="font-medium hover:text-amber-300">
                  {order.item.product_title}
                </Link>
                <p className="mt-1 text-sm text-zinc-400">{order.item.sku_spec || '默认规格'} · {order.item.quantity} 件</p>
              </div>
              <span className="text-sm text-amber-300">{labels[order.status]}</span>
            </div>
            <div className="mt-3 flex flex-wrap items-center justify-between gap-2 border-t border-zinc-800 pt-3 text-sm">
              <div>
                <p>¥{(order.total_amount / 100).toFixed(2)}</p>
                <p className="text-xs text-zinc-500">订单 {order.order_no}</p>
              </div>
              <div className="flex gap-2">
                {order.source && <Link to="/video/$id" params={{ id: String(order.source.video_id) }} className="self-center text-amber-300">来源视频</Link>}
                {!merchant && order.status === 'PENDING_PAYMENT' && <>
                  <Button size="sm" disabled={action.isPending} onClick={() => action.mutate({ order, kind: 'pay' })}>模拟支付</Button>
                  <Button size="sm" variant="outline" disabled={action.isPending} onClick={() => action.mutate({ order, kind: 'cancel' })}>取消</Button>
                </>}
                {merchant && order.status === 'PAID' && <Button size="sm" disabled={action.isPending} onClick={() => action.mutate({ order, kind: 'fulfill' })}>模拟发货</Button>}
                {!merchant && order.status === 'FULFILLING' && <Button size="sm" disabled={action.isPending} onClick={() => action.mutate({ order, kind: 'complete' })}>确认收货</Button>}
              </div>
            </div>
          </Card>
        ))}
      </div>
      {orders.hasNextPage && <Button className="mt-5" variant="outline" disabled={orders.isFetchingNextPage} onClick={() => orders.fetchNextPage()}>
        {orders.isFetchingNextPage ? '加载中…' : '加载更多'}
      </Button>}
    </div>
  )
}
