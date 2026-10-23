import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { z } from 'zod'
import { chapterIds, type ChapterId } from '../features/learn/chapters'
import { LearnPage } from '../features/learn/learn-page'

const learnSearchSchema = z.object({
  chapter: z.enum(chapterIds).optional(),
})

export const Route = createFileRoute('/learn')({
  validateSearch: learnSearchSchema,
  component: LearnRoute,
})

function LearnRoute() {
  const { chapter } = Route.useSearch()
  const navigate = useNavigate()
  return (
    <LearnPage
      chapter={chapter}
      onChapter={(id: ChapterId) => {
        navigate({ to: '/learn', search: { chapter: id }, replace: true })
      }}
    />
  )
}
