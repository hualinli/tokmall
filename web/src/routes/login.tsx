import { createFileRoute } from '@tanstack/react-router'

export const Route = createFileRoute('/login')({
  component: LoginPage,
})

function LoginPage() {
  return (
    <div className="py-16 text-center text-zinc-400">
      <p className="text-2xl font-semibold text-zinc-200">登录占位</p>
      <p className="mt-2 text-sm">F1 里程碑接入 /auth/login + token 生命周期</p>
    </div>
  )
}
