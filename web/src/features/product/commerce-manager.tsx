import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useRef, useState } from 'react'
import { toast } from 'sonner'
import { client } from '../../api/client'
import { Button } from '../../components/ui/button'
import { Card } from '../../components/ui/card'
import { apiErrorMessage } from '../../lib/api-error'

/** Minimal merchant setup for the local commerce demo. The author/merchant
 * identity overlap is the explicit v1 promotion authorization shortcut. */
export function CommerceManager() {
  const qc = useQueryClient()
  const [title, setTitle] = useState('')
  const [productId, setProductId] = useState(0)
  const [spec, setSpec] = useState('')
  const [price, setPrice] = useState('')
  const [skuId, setSkuId] = useState(0)
  const [stock, setStock] = useState('')
  const stockRequest = useRef({ fingerprint: '', key: '' })

  const products = useQuery({
    queryKey: ['my-products'],
    queryFn: async () => {
      const res = await client.GET('/api/v1/products', { params: { query: { limit: 100 } } })
      if (res.error) throw new Error(apiErrorMessage(res.error))
      return res.data
    },
  })
  const selected = productId || products.data?.items[0]?.product_id || 0
  const detail = useQuery({
    queryKey: ['product', String(selected)],
    enabled: selected > 0,
    queryFn: async () => {
      const res = await client.GET('/api/v1/products/{id}', { params: { path: { id: selected } } })
      if (res.error) throw new Error(apiErrorMessage(res.error))
      return res.data
    },
  })

  const create = useMutation({
    mutationFn: async () => {
      const res = await client.POST('/api/v1/products', { body: { title: title.trim() } })
      if (res.error) throw new Error(apiErrorMessage(res.error))
      return res.data
    },
    onSuccess: (item) => {
      setProductId(item.product_id)
      setTitle('')
      qc.invalidateQueries({ queryKey: ['my-products'] })
      toast.success('商品已创建')
    },
    onError: (error) => toast.error(error.message),
  })
  const addSKU = useMutation({
    mutationFn: async () => {
      const cents = Math.round(Number(price) * 100)
      if (!Number.isSafeInteger(cents) || cents <= 0) throw new Error('请输入有效价格')
      const res = await client.POST('/api/v1/products/{id}/skus', {
        params: { path: { id: selected } }, body: { spec: spec.trim(), price: cents },
      })
      if (res.error) throw new Error(apiErrorMessage(res.error))
      return res.data
    },
    onSuccess: (sku) => {
      setSkuId(sku.sku_id)
      setSpec('')
      setPrice('')
      qc.invalidateQueries({ queryKey: ['product', String(selected)] })
      toast.success('规格已创建')
    },
    onError: (error) => toast.error(error.message),
  })
  const restock = useMutation({
    mutationFn: async () => {
      const amount = Number(stock)
      if (!Number.isSafeInteger(amount) || amount <= 0) throw new Error('请输入正整数库存')
      const effectiveSKU = skuId || detail.data?.skus[0]?.sku_id || 0
      if (!effectiveSKU) throw new Error('请先添加规格')
      const fingerprint = `${selected}:${effectiveSKU}:${amount}`
      if (stockRequest.current.fingerprint !== fingerprint) {
        stockRequest.current = { fingerprint, key: crypto.randomUUID() }
      }
      const res = await client.POST('/api/v1/products/{id}/skus/{sku_id}/stock', {
        params: { path: { id: selected, sku_id: effectiveSKU } },
        body: { change: amount, idempotency_key: stockRequest.current.key },
      })
      if (res.error) throw new Error(apiErrorMessage(res.error))
      return res.data
    },
    onSuccess: (result) => {
      setStock('')
      stockRequest.current = { fingerprint: '', key: '' }
      toast.success(`当前库存 ${result.quantity}`)
    },
    onError: (error) => toast.error(error.message),
  })

  return (
    <Card className="mt-6 space-y-4">
      <h2 className="font-semibold">我的商品</h2>
      <p className="text-xs text-zinc-500">先创建商品与规格、补库存，再到已发布视频挂商品。</p>
      <div className="flex gap-2">
        <input value={title} onChange={(e) => setTitle(e.target.value)} placeholder="商品名称" className="min-w-0 flex-1 rounded-lg border border-zinc-700 bg-zinc-900 p-2" />
        <Button disabled={!title.trim() || create.isPending} onClick={() => create.mutate()}>创建商品</Button>
      </div>
      {(products.data?.items.length ?? 0) > 0 && (
        <>
          <select value={selected} onChange={(e) => { setProductId(Number(e.target.value)); setSkuId(0) }} className="w-full rounded-lg border border-zinc-700 bg-zinc-900 p-2">
            {products.data?.items.map((item) => <option key={item.product_id} value={item.product_id}>{item.title} #{item.product_id}</option>)}
          </select>
          <div className="grid gap-2 sm:grid-cols-[1fr_1fr_auto]">
            <input value={spec} onChange={(e) => setSpec(e.target.value)} placeholder="规格（可空）" className="rounded-lg border border-zinc-700 bg-zinc-900 p-2" />
            <input value={price} onChange={(e) => setPrice(e.target.value)} type="number" min="0.01" step="0.01" placeholder="价格（元）" className="rounded-lg border border-zinc-700 bg-zinc-900 p-2" />
            <Button disabled={!price || addSKU.isPending} onClick={() => addSKU.mutate()}>添加规格</Button>
          </div>
          {(detail.data?.skus.length ?? 0) > 0 && (
            <div className="grid gap-2 sm:grid-cols-[1fr_1fr_auto]">
              <select value={skuId || detail.data?.skus[0].sku_id} onChange={(e) => setSkuId(Number(e.target.value))} className="rounded-lg border border-zinc-700 bg-zinc-900 p-2">
                {detail.data?.skus.map((sku) => <option key={sku.sku_id} value={sku.sku_id}>{sku.spec || '默认规格'} · ¥{(sku.price / 100).toFixed(2)}</option>)}
              </select>
              <input value={stock} onChange={(e) => setStock(e.target.value)} type="number" min="1" step="1" placeholder="入库数量" className="rounded-lg border border-zinc-700 bg-zinc-900 p-2" />
              <Button disabled={!stock || restock.isPending} onClick={() => restock.mutate()}>补库存</Button>
            </div>
          )}
        </>
      )}
    </Card>
  )
}
