import { useForm } from 'react-hook-form'
import { zodResolver } from '@hookform/resolvers/zod'
import { z } from 'zod'
import { useRouter } from '@tanstack/react-router'
import { client } from '../../api/client'
import { useAuthStore } from '../../lib/auth-store'
import { apiErrorMessage } from '../../lib/api-error'
import { safeRedirectPath } from '../../lib/safe-redirect'
import { Button } from '../../components/ui/button'
import { Input } from '../../components/ui/input'
import { Card, CardTitle, Field } from '../../components/ui/card'

// 校验规则与 OpenAPI 契约对齐（username pattern / password 8-72）
const registerSchema = z.object({
  email: z.string().email('邮箱格式不正确'),
  username: z
    .string()
    .regex(/^[a-zA-Z][a-zA-Z0-9]{5,15}$/, '字母开头，5-15 位字母数字'),
  password: z
    .string()
    .min(8, '至少 8 位')
    .max(72, '最多 72 位'),
})
type RegisterValues = z.infer<typeof registerSchema>

export function RegisterForm({ redirect }: { redirect?: string }) {
  const router = useRouter()
  const setAuth = useAuthStore((s) => s.setAuth)
  const {
    register,
    handleSubmit,
    setError,
    formState: { errors, isSubmitting },
  } = useForm<RegisterValues>({ resolver: zodResolver(registerSchema) })

  async function onSubmit(values: RegisterValues) {
    const res = await client.POST('/api/v1/auth/register', { body: values })
    if (res.error) {
      setError('root', { message: apiErrorMessage(res.error) })
      return
    }
    // 注册即签发 token 对：直接进入登录态
    setAuth(res.data)
    // 同 LoginForm：redirect 是外部输入，必须过同源校验后才跳转
    const target = safeRedirectPath(redirect)
    if (target) {
      router.history.push(target)
    } else {
      router.navigate({ to: '/' })
    }
  }

  return (
    <Card className="w-full max-w-sm">
      <CardTitle>注册 TokMall</CardTitle>
      <form onSubmit={handleSubmit(onSubmit)} className="mt-6 space-y-4">
        <Field label="邮箱" error={errors.email?.message}>
          <Input
            type="email"
            autoComplete="email"
            placeholder="you@example.com"
            {...register('email')}
          />
        </Field>
        <Field
          label="用户名"
          hint="字母开头，5-15 位字母数字"
          error={errors.username?.message}
        >
          <Input
            autoComplete="username"
            placeholder="tok_fan_2026"
            {...register('username')}
          />
        </Field>
        <Field
          label="密码"
          hint="8-72 位"
          error={errors.password?.message}
        >
          <Input
            type="password"
            autoComplete="new-password"
            placeholder="••••••••"
            {...register('password')}
          />
        </Field>
        {errors.root?.message && (
          <p className="text-sm text-red-400">{errors.root.message}</p>
        )}
        <Button type="submit" size="lg" className="w-full" disabled={isSubmitting}>
          {isSubmitting ? '注册中…' : '注册'}
        </Button>
      </form>
    </Card>
  )
}
