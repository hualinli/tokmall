import { createFileRoute, Link } from '@tanstack/react-router'
import { z } from 'zod'
import { RegisterForm } from '../features/auth/register-form'

const registerSearchSchema = z.object({
  redirect: z.string().optional(),
})

export const Route = createFileRoute('/register')({
  validateSearch: registerSearchSchema,
  component: RegisterPage,
})

function RegisterPage() {
  const { redirect } = Route.useSearch()
  return (
    <div className="flex w-full flex-1 flex-col items-center justify-center py-16">
      <RegisterForm redirect={redirect} />
      <p className="mt-4 text-sm text-zinc-500">
        已有账号？{' '}
        <Link to="/login" className="text-zinc-300 hover:underline">
          去登录
        </Link>
      </p>
    </div>
  )
}
