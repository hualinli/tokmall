import { createFileRoute, Link } from '@tanstack/react-router'
import { useMutation, useQuery } from '@tanstack/react-query'
import { useRef, useState } from 'react'
import { toast } from 'sonner'
import { z } from 'zod'
import { client } from '../api/client'
import { Button } from '../components/ui/button'
import { apiErrorMessage } from '../lib/api-error'
import { useAuthStore } from '../lib/auth-store'

const searchSchema = z.object({
  video_id: z.number().int().positive().optional(),
  author_id: z.number().int().positive().optional(),
})

export const Route = createFileRoute('/product/$id')({
  validateSearch: searchSchema,
  component: ProductPage,
})

function ProductPage() {
  const { id } = Route.useParams()
  const { video_id, author_id } = Route.useSearch()
  const productId = Number(id)
  const accessToken = useAuthStore((s) => s.accessToken)
  const [skuId, setSkuId] = useState(0)
  const [quantity, setQuantity] = useState(1)
  const [paidOrder, setPaidOrder] = useState<number | null>(null)
  const orderKey = useRef({ key: crypto.randomUUID(), fingerprint: '' })
  const paymentIdentity = useRef<{ orderId: number; paymentNo: string; tradeNo: string } | null>(null)

  const product = useQuery({
    queryKey: ['product', id],
    queryFn: async () => {
      const res = await client.GET('/api/v1/products/{id}', { params: { path: { id: productId } } })
      if (res.error) throw new Error(apiErrorMessage(res.error))
      return res.data
    },
  })

  const checkout = useMutation({
    mutationFn: async () => {
      const effectiveSKU = skuId || product.data?.skus[0]?.sku_id || 0
      const fingerprint = `${productId}:${effectiveSKU}:${quantity}:${video_id ?? 0}:${author_id ?? 0}`
      if (orderKey.current.fingerprint !== fingerprint) {
        orderKey.current = { key: crypto.randomUUID(), fingerprint }
        paymentIdentity.current = null
      }
      const source = video_id && author_id
        ? { video_id, author_id, product_id: productId }
        : undefined
      const created = await client.POST('/api/v1/orders', {
        body: { sku_id: effectiveSKU, quantity, idempotency_key: orderKey.current.key, source },
      })
      if (created.error) throw new Error(apiErrorMessage(created.error))
      const order = created.data
      if (!paymentIdentity.current || paymentIdentity.current.orderId !== order.order_id) {
        paymentIdentity.current = {
          orderId: order.order_id,
          paymentNo: crypto.randomUUID(),
          tradeNo: crypto.randomUUID(),
        }
      }
      const payment = await client.POST('/api/v1/orders/{id}/pay', {
        params: { path: { id: order.order_id } },
        body: {
          payment_no: paymentIdentity.current.paymentNo,
          provider_trade_no: paymentIdentity.current.tradeNo,
          amount: order.total_amount,
        },
      })
      if (payment.error) throw new Error(apiErrorMessage(payment.error))
      return order.order_id
    },
    onSuccess: (orderId) => {
      setPaidOrder(orderId)
      toast.success('模拟支付成功')
    },
    onError: (error) => toast.error(error.message),
  })

  if (product.isPending) return <p className="mx-auto max-w-2xl p-8 text-zinc-400">商品加载中…</p>
  if (product.error || !product.data) {
    return <p className="mx-auto max-w-2xl p-8 text-red-400">{product.error?.message ?? '商品不存在'}</p>
  }
  const item = product.data
  const selectedSKU = item.skus.find((sku) => sku.sku_id === skuId) ?? item.skus[0]

  return (
    <div className="mx-auto grid max-w-3xl gap-8 px-4 py-8 md:grid-cols-2">
      <div className="aspect-square overflow-hidden rounded-xl bg-zinc-900">
        {item.cover_url && <img src={item.cover_url} alt={item.title} className="h-full w-full object-cover" />}
      </div>
      <div>
        <h1 className="text-2xl font-semibold">{item.title}</h1>
        <p className="mt-3 whitespace-pre-wrap text-zinc-400">{item.description}</p>
        {video_id && author_id && (
          <Link to="/video/$id" params={{ id: String(video_id) }} className="mt-3 block text-sm text-amber-300 hover:underline">
            返回推荐视频
          </Link>
        )}
        <p className="mt-6 text-xl text-amber-300">
          {selectedSKU ? `¥${(selectedSKU.price / 100).toFixed(2)}` : '暂无可售规格'}
        </p>
        {item.status !== 'ON_SALE' && <p className="mt-3 text-red-400">商品已下架</p>}
        {item.skus.length > 0 && (
          <label className="mt-5 block text-sm">
            规格
            <select
              value={selectedSKU?.sku_id ?? ''}
              onChange={(event) => setSkuId(Number(event.target.value))}
              className="mt-2 block w-full rounded-lg border border-zinc-700 bg-zinc-900 p-2"
            >
              {item.skus.map((sku) => <option key={sku.sku_id} value={sku.sku_id}>{sku.spec || '默认规格'}</option>)}
            </select>
          </label>
        )}
        <label className="mt-4 block text-sm">
          数量
          <input
            type="number" min={1} max={1000} value={quantity}
            onChange={(event) => setQuantity(Math.max(1, Math.min(1000, Number(event.target.value) || 1)))}
            className="mt-2 block w-full rounded-lg border border-zinc-700 bg-zinc-900 p-2"
          />
        </label>
        {paidOrder ? (
          <p className="mt-6 rounded-lg bg-emerald-950 p-3 text-emerald-300">
            订单 #{paidOrder} 已模拟支付成功 · <Link to="/orders" className="underline">查看订单</Link>
          </p>
        ) : accessToken ? (
          <Button
            className="mt-6 w-full" disabled={!selectedSKU || item.status !== 'ON_SALE' || checkout.isPending}
            onClick={() => checkout.mutate()}
          >
            {checkout.isPending ? '处理中…' : '下单并模拟支付'}
          </Button>
        ) : (
          <Link to="/login" search={{ redirect: window.location.pathname + window.location.search }} className="mt-6 block">
            <Button className="w-full">登录后购买</Button>
          </Link>
        )}
        <p className="mt-3 text-xs text-zinc-500">学习项目：这里执行模拟支付，不会收取真实款项。</p>
      </div>
    </div>
  )
}
