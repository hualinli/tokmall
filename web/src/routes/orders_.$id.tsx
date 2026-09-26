import { createFileRoute, Link, redirect } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { z } from 'zod'
import { client } from '../api/client'
import { Card } from '../components/ui/card'
import { apiErrorMessage } from '../lib/api-error'

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
  const order = useQuery({
    queryKey: ['order', merchant, id],
    queryFn: async () => {
      const params = { path: { id: orderId } }
      const res = merchant
        ? await client.GET('/api/v1/merchant/orders/{id}', { params })
        : await client.GET('/api/v1/orders/{id}', { params })
      if (res.error) throw new Error(apiErrorMessage(res.error))
      return res.data
    },
  })

  return (
    <div className="mx-auto max-w-2xl px-4 py-8">
      <Link to="/orders" className="text-sm text-amber-300 hover:underline">← 返回订单列表</Link>
      {order.isPending && <p className="mt-6 text-zinc-400">订单加载中…</p>}
      {order.error && <p className="mt-6 text-red-400">{order.error.message}</p>}
      {order.data && <Card className="mt-5 space-y-3 p-5">
        <h1 className="text-xl font-semibold">订单详情</h1>
        <p className="break-all text-sm text-zinc-500">{order.data.order_no}</p>
        <p>状态：{order.data.status}</p>
        <p>商品：{order.data.item.product_title}</p>
        <p>规格：{order.data.item.sku_spec || '默认规格'}</p>
        <p>数量：{order.data.item.quantity}</p>
        <p>单价：¥{(order.data.item.unit_price / 100).toFixed(2)}</p>
        <p className="font-semibold">合计：¥{(order.data.total_amount / 100).toFixed(2)}</p>
        <div className="border-t border-zinc-800 pt-3 text-sm text-zinc-400">
          <p>创建：{new Date(order.data.created_at).toLocaleString()}</p>
          {order.data.paid_at && <p>支付：{new Date(order.data.paid_at).toLocaleString()}</p>}
          {order.data.fulfilling_at && <p>发货：{new Date(order.data.fulfilling_at).toLocaleString()}</p>}
          {order.data.completed_at && <p>完成：{new Date(order.data.completed_at).toLocaleString()}</p>}
          {order.data.canceled_at && <p>取消：{new Date(order.data.canceled_at).toLocaleString()}</p>}
          {order.data.expired_at && <p>超时：{new Date(order.data.expired_at).toLocaleString()}</p>}
        </div>
      </Card>}
    </div>
  )
}
