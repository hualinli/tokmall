import { createFileRoute } from '@tanstack/react-router'

export const Route = createFileRoute('/register')({
  component: RegisterPage,
})

function RegisterPage() {
  return (
    <div className="py-16 text-center text-zinc-400">
      <p className="text-2xl font-semibold text-zinc-200">注册占位</p>
      <p className="mt-2 text-sm">F1 里程碑接入 /auth/register</p>
    </div>
  )
}
