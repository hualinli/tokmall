import { createFileRoute, Link } from '@tanstack/react-router'
import { z } from 'zod'
import { LoginForm } from '../features/auth/login-form'

const loginSearchSchema = z.object({
  // 未登录点赞/关注/进入受保护页时回跳地址
  redirect: z.string().optional(),
})

export const Route = createFileRoute('/login')({
  validateSearch: loginSearchSchema,
  component: LoginPage,
})

function LoginPage() {
  const { redirect } = Route.useSearch()
  return (
    <div className="flex w-full flex-1 flex-col items-center justify-center py-16">
      <LoginForm redirect={redirect} />
      <p className="mt-4 text-sm text-zinc-500">
        还没有账号？{' '}
        <Link to="/register" className="text-zinc-300 hover:underline">
          去注册
        </Link>
      </p>
    </div>
  )
}
