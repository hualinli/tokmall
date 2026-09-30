import { useInfiniteQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { toast } from 'sonner'
import { client } from '../../api/client'
import { Button } from '../../components/ui/button'
import { apiRequestError } from '../../lib/api-error'
import type { components } from '../../api/types'

type Card = components['schemas']['VideoProductCard']

/** 与 commerce-manager 一致：后端上限 50，契约与前端都不再写 100 */
const PRODUCT_PAGE_SIZE = 50

export function VideoPromotionManager({ videoId, cards }: { videoId: number; cards: Card[] }) {
  const qc = useQueryClient()
  const [productId, setProductId] = useState(0)
  // key 必须含 status：不带 status 的商家管理列表（commerce-manager）是另一份缓存
  const products = useInfiniteQuery({
    queryKey: ['my-products', 'on-sale', PRODUCT_PAGE_SIZE],
    initialPageParam: undefined as number | undefined,
    queryFn: async ({ pageParam }) => {
      const res = await client.GET('/api/v1/products', {
        params: {
          query: { limit: PRODUCT_PAGE_SIZE, status: 'ON_SALE', before_id: pageParam },
        },
      })
      if (res.error) throw apiRequestError(res)
      return res.data
    },
    getNextPageParam: (last) => (last.has_more ? (last.next_before_id ?? undefined) : undefined),
  })
  const items = products.data?.pages.flatMap((page) => page.items) ?? []
  const link = useMutation({
    mutationFn: async () => {
      const selected = productId || items[0]?.product_id || 0
      const res = await client.POST('/api/v1/videos/{id}/products', {
        params: { path: { id: videoId } },
        body: { product_id: selected, sort_order: cards.length },
      })
      if (res.error) throw apiRequestError(res)
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
      if (res.error) throw apiRequestError(res)
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
        <select value={productId || items[0]?.product_id || ''} onChange={(e) => setProductId(Number(e.target.value))} className="min-w-0 flex-1 rounded-lg border border-zinc-700 bg-zinc-900 p-2">
          {items.map((item) => <option key={item.product_id} value={item.product_id}>{item.title}</option>)}
        </select>
        <Button disabled={!items.length || cards.length >= 5 || link.isPending} onClick={() => link.mutate()}>挂商品</Button>
      </div>
      {products.hasNextPage && (
        <Button variant="outline" size="sm" className="mt-2" disabled={products.isFetchingNextPage} onClick={() => products.fetchNextPage()}>
          {products.isFetchingNextPage ? '加载中…' : '加载更多商品'}
        </Button>
      )}
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
