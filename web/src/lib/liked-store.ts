import { create } from 'zustand'

/**
 * Session 级"我赞过的视频"内存表。
 * 为什么需要：公开 Feed 契约不含 is_liked（后端刻意不做个性化），
 * 但点赞后 UI 需要立刻保持"已赞"状态；刷新页面后清空（与后端一致）。
 * 详情页的 is_liked 来自服务端，本表只是叠加层。
 */
interface LikedStore {
  liked: Record<number, boolean>
  set: (videoId: number, liked: boolean) => void
}

export const useLikedStore = create<LikedStore>()((set) => ({
  liked: {},
  set: (videoId, liked) =>
    set((s) => ({ liked: { ...s.liked, [videoId]: liked } })),
}))
