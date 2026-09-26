import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { toast } from 'sonner'
import { client } from '../../api/client'
import { Button } from '../../components/ui/button'
import { apiErrorMessage } from '../../lib/api-error'
import type { components } from '../../api/types'

type Card = components['schemas']['VideoProductCard']

export function VideoPromotionManager({ videoId, cards }: { videoId: number; cards: Card[] }) {
  const qc = useQueryClient()
  const [productId, setProductId] = useState(0)
  const products = useQuery({
    queryKey: ['my-products'],
    queryFn: async () => {
      const res = await client.GET('/api/v1/products', { params: { query: { limit: 100, status: 'ON_SALE' } } })
      if (res.error) throw new Error(apiErrorMessage(res.error))
      return res.data
    },
  })
  const link = useMutation({
    mutationFn: async () => {
      const selected = productId || products.data?.items[0]?.product_id || 0
      const res = await client.POST('/api/v1/videos/{id}/products', {
        params: { path: { id: videoId } },
        body: { product_id: selected, sort_order: cards.length },
      })
      if (res.error) throw new Error(apiErrorMessage(res.error))
      return res.data
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['video', String(videoId)] })
      qc.invalidateQueries({ queryKey: ['feed'] })
      toast.success('商品已挂到视频')
    },
    onError: (error) => toast.error(error.message),
  })
  const unlink = useMutation({
    mutationFn: async (id: number) => {
      const res = await client.DELETE('/api/v1/videos/{id}/products/{product_id}', {
        params: { path: { id: videoId, product_id: id } },
      })
      if (res.error) throw new Error(apiErrorMessage(res.error))
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['video', String(videoId)] })
      qc.invalidateQueries({ queryKey: ['feed'] })
      toast.success('已取消挂载')
    },
    onError: (error) => toast.error(error.message),
  })

  return (
    <section className="mt-6 rounded-xl border border-zinc-800 p-4">
      <h2 className="font-semibold">管理视频商品</h2>
      <p className="mt-1 text-xs text-zinc-500">最多挂 5 件自己的在售商品。可先在个人页创建商品与规格。</p>
      <div className="mt-3 flex gap-2">
        <select value={productId || products.data?.items[0]?.product_id || ''} onChange={(e) => setProductId(Number(e.target.value))} className="min-w-0 flex-1 rounded-lg border border-zinc-700 bg-zinc-900 p-2">
          {products.data?.items.map((item) => <option key={item.product_id} value={item.product_id}>{item.title}</option>)}
        </select>
        <Button disabled={!products.data?.items.length || cards.length >= 5 || link.isPending} onClick={() => link.mutate()}>挂商品</Button>
      </div>
      <ul className="mt-3 space-y-2 text-sm">
        {cards.map((card) => (
          <li key={card.product_id} className="flex items-center justify-between gap-2">
            <span className="truncate">{card.title}</span>
            <Button variant="outline" size="sm" disabled={unlink.isPending} onClick={() => unlink.mutate(card.product_id)}>取消挂载</Button>
          </li>
        ))}
      </ul>
    </section>
  )
}
