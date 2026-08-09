import { useForm } from 'react-hook-form'
import { zodResolver } from '@hookform/resolvers/zod'
import { z } from 'zod'
import { useRouter } from '@tanstack/react-router'
import { client } from '../../api/client'
import { useAuthStore } from '../../lib/auth-store'
import { apiErrorMessage } from '../../lib/api-error'
import { Button } from '../../components/ui/button'
import { Input } from '../../components/ui/input'
import { Card, CardTitle, Field } from '../../components/ui/card'

const loginSchema = z.object({
  identifier: z.string().min(1, '请输入邮箱或用户名'),
  password: z.string().min(1, '请输入密码'),
})
type LoginValues = z.infer<typeof loginSchema>

export function LoginForm({ redirect }: { redirect?: string }) {
  const router = useRouter()
  const setAuth = useAuthStore((s) => s.setAuth)
  const {
    register,
    handleSubmit,
    setError,
    formState: { errors, isSubmitting },
  } = useForm<LoginValues>({ resolver: zodResolver(loginSchema) })

  async function onSubmit(values: LoginValues) {
    const res = await client.POST('/api/v1/auth/login', { body: values })
    if (res.error) {
      setError('root', {
        message:
          res.error.error.code === 'UNAUTHENTICATED'
            ? '账号或密码错误'
            : apiErrorMessage(res.error),
      })
      return
    }
    setAuth(res.data)
    // 回跳来源页（点赞/关注/受保护页触发登录时）；否则回首页
    if (redirect?.startsWith('/')) {
      router.history.push(redirect)
    } else {
      router.navigate({ to: '/' })
    }
  }

  return (
    <Card className="w-full max-w-sm">
      <CardTitle>登录 TokMall</CardTitle>
      <form onSubmit={handleSubmit(onSubmit)} className="mt-6 space-y-4">
        <Field label="邮箱或用户名" error={errors.identifier?.message}>
          <Input
            autoComplete="username"
            placeholder="you@example.com / username"
            {...register('identifier')}
          />
        </Field>
        <Field label="密码" error={errors.password?.message}>
          <Input
            type="password"
            autoComplete="current-password"
            placeholder="••••••••"
            {...register('password')}
          />
        </Field>
        {errors.root?.message && (
          <p className="text-sm text-red-400">{errors.root.message}</p>
        )}
        <Button type="submit" size="lg" className="w-full" disabled={isSubmitting}>
          {isSubmitting ? '登录中…' : '登录'}
        </Button>
      </form>
    </Card>
  )
}
