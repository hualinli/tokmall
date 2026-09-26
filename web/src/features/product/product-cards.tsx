import { Link } from '@tanstack/react-router'
import type { components } from '../../api/types'

type Card = components['schemas']['VideoProductCard']

export function ProductCards({ cards, videoId, authorId }: { cards: Card[]; videoId: number; authorId: number }) {
  if (cards.length === 0) return null
  return (
    <div className="flex gap-2 overflow-x-auto py-2" aria-label="视频商品">
      {cards.map((card) => (
        <Link
          key={card.product_id}
          to="/product/$id"
          params={{ id: String(card.product_id) }}
          search={{ video_id: videoId, author_id: authorId }}
          className="flex min-w-40 max-w-56 items-center gap-2 rounded-lg bg-zinc-900/90 p-2 text-sm hover:bg-zinc-800"
        >
          {card.cover_url ? (
            <img src={card.cover_url} alt="" className="h-10 w-10 rounded object-cover" />
          ) : (
            <span className="h-10 w-10 shrink-0 rounded bg-zinc-700" />
          )}
          <span className="min-w-0">
            <span className="block truncate">{card.title}</span>
            <span className="text-amber-300">¥{(card.min_price / 100).toFixed(2)}</span>
          </span>
        </Link>
      ))}
    </div>
  )
}
