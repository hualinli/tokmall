export const experiments = [
  {
    id: 'replay',
    title: '超时后原样重试',
    request: '用户 7 · key=buy-1 · SKU 11 · 数量 1',
    setup: '首单已提交，但响应丢失。库存剩 2 件，已有订单 O-001。',
    answer: 'replay',
    frames: [
      ['收到重试', '浏览器不知道首单是否提交，沿用原来的 key 和业务载荷。'],
      [
        '等待同键锁',
        '按用户 7 + buy-1 获取事务级咨询锁。同键并发会在这里等待；不同实例也共享 PG 锁。',
      ],
      [
        '比较指纹',
        '找到 O-001，SKU、数量、来源和券均与原请求一致。无需重新校验商品或已被占用的券。',
      ],
      [
        '重放原单',
        '返回 O-001，Replayed=true。没有新订单、流水或超时事件，库存仍为 2。',
      ],
    ],
    result: '订单总数 1 · 扣减流水 1 · 超时事件 1 · 库存 2',
    why: '网络超时只说明响应没拿到，不能据此判断事务失败。重试必须保留同一业务尝试的 key。',
  },
  {
    id: 'conflict',
    title: '同键改成买两件',
    request: '用户 7 · key=buy-1 · SKU 11 · 数量 2',
    setup: '首单数量为 1，已提交。库存剩 2 件，已有订单 O-001。',
    answer: 'conflict',
    frames: [
      ['收到修改后的请求', 'key 没变，数量却从 1 改成了 2。'],
      ['获取同键锁', '仍按用户 7 + buy-1 串行化，并读到原订单。'],
      [
        '指纹不同',
        '数量属于业务事实，参与 requestFingerprint。新旧指纹不一致。',
      ],
      [
        '拒绝请求',
        '返回 ErrIdempotencyConflict。原单数量仍为 1，不把重放伪装成修改成功。',
      ],
    ],
    result: '订单总数 1 · 扣减流水 1 · 超时事件 1 · 库存 2',
    why: '同键意味着同一次尝试，不是修改订单的命令。确认要重新购买时才建立新尝试。',
  },
  {
    id: 'new-key',
    title: '重试时换一个键',
    request: '用户 7 · key=buy-2 · SKU 11 · 数量 1',
    setup: '首单已提交。库存剩 2 件，无优惠券，商品仍在售。',
    answer: 'create',
    frames: [
      ['使用新键', 'buy-2 与 buy-1 属于不同尝试，不会命中原订单。'],
      [
        '准备新订单',
        '校验服务端 SKU 与价格，插入订单、明细、扣减流水和超时事件；此时尚未提交。',
      ],
      [
        '条件扣库存',
        'quantity >= 1 成立，库存从 2 变成 1；热点行在事务末尾才被修改。',
      ],
      [
        '提交新单',
        '提交 O-002。两个 key 允许两张订单，业务不能靠幂等机制判断你是否误点。',
      ],
    ],
    result: '订单总数 2 · 扣减流水 2 · 超时事件 2 · 库存 1',
    why: '每次重试都生成新 key 会失去幂等保护。应把 key 与业务尝试绑定，而非与 HTTP 请求绑定。',
  },
  {
    id: 'other-user',
    title: '另一个用户使用同键',
    request: '用户 8 · key=buy-1 · SKU 11 · 数量 1',
    setup: '用户 7 已买一件。库存剩 2 件，无优惠券，商品仍在售。',
    answer: 'create',
    frames: [
      [
        '切换用户',
        '用户身份来自认证中间件，不能由浏览器随意指定。这里只切换教学角色。',
      ],
      [
        '不同幂等范围',
        '锁与唯一约束均包含 user_id。用户 8 查不到用户 7 的同键原单。',
      ],
      [
        '准备并扣减',
        '为用户 8 写入自己的订单、明细、流水和超时事件，条件扣减一件。',
      ],
      [
        '提交新单',
        '提交 O-002。用户 7 的 O-001 保持不变，用户 8 不会获得别人的订单。',
      ],
    ],
    result: '订单总数 2 · 扣减流水 2 · 超时事件 2 · 库存 1',
    why: '幂等键的作用域是 (user_id, idempotency_key)。只按 key 查单会产生跨用户冲突甚至数据泄露。',
  },
  {
    id: 'rollback',
    title: '最后扣库存时失败',
    request: '用户 7 · key=buy-3 · SKU 11 · 数量 2 · 有效券 C-1',
    setup: '独立实验：库存只有 1 件，尚无订单，券为 UNUSED，券后应付大于 0。',
    answer: 'rollback',
    frames: [
      ['开启事务', '新 key 没有原单，校验 SKU 并锁住持券行。'],
      [
        '写入未提交的事实',
        '插订单、明细、扣减流水，核销券，写入超时事件。它们目前只在本事务内生效。',
      ],
      [
        '扣减失败',
        'UPDATE 的 quantity >= 2 不成立，影响行数为 0。库存行存在，返回 ErrInsufficientStock。',
      ],
      [
        '整体回滚',
        '事务内新订单、明细、流水、超时事件全部撤销，券回到 UNUSED；库存仍为 1。',
      ],
    ],
    result: '订单总数 0 · 扣减流水 0 · 超时事件 0 · 库存 1 · 券 UNUSED',
    why: '“先写订单、最后扣库存”成立的前提是同库同事务。把其中一次写入移出事务，就会留下残缺事实。',
  },
] as const

export const outcomes = [
  { id: 'replay', label: '返回原订单' },
  { id: 'conflict', label: '幂等冲突' },
  { id: 'create', label: '创建新订单' },
  { id: 'rollback', label: '失败并整体回滚' },
] as const

export const questions = [
  {
    title: '同键、同券的两个请求同时到达，先做什么？',
    options: [
      '先检查券是不是 UNUSED',
      '先按用户和幂等键加锁，再查原订单',
      '先扣库存，避免别人抢走',
    ],
    answer: 1,
    explanation:
      '如果先校验券，第二个请求可能把首单占用的券误判为失败。同键锁让它等待，再读到已提交原单并重放。',
  },
  {
    title: '已有唯一约束，为什么还需要比较指纹？',
    options: [
      '唯一约束只防重复键，不能证明业务载荷相同',
      '有唯一约束就不必比较',
      '指纹可以防止库存超卖',
    ],
    answer: 0,
    explanation:
      '唯一约束回答“这对用户和键是否已存在”。指纹回答“是否同一次业务事实”。库存并发安全由条件扣减和数据库约束承担。',
  },
  {
    title: '教程动画通过，可以证明真实 PG 并发安全吗？',
    options: [
      '可以，动画就是数据库执行结果',
      '可以，只要前端禁止重复点击',
      '还需真实 PG 集成测试，检查订单、余额与流水',
    ],
    answer: 2,
    explanation:
      '动画是教学模型。service 的 fake repo 测试也不能证明真实行锁与回滚，需要显式开启隔离数据库集成测试。',
  },
] as const

export const tasks = [
  {
    title: '任务一：补充“同键换券”的指纹测试',
    instruction:
      '在 internal/order/service_test.go 增加用例：固定用户、SKU、数量和 key，仅把 UserCouponID 从 10 改为 11。先自己写，再看提示。',
    hint: '复用 fakeOrderRepository，第一次 Create 成功后复制参数，换券再调用；用 errors.Is 或 require.ErrorIs 判断领域错误。',
    reference: `p := CreateParams{UserID: 3, SKUID: 11, Quantity: 1,
    IdempotencyKey: "same", UserCouponID: &couponA}
_, err := svc.Create(ctx, p) // 使用测试已有的 Service 和 fake repo
require.NoError(t, err)
p.UserCouponID = &couponB
_, err = svc.Create(ctx, p)
require.ErrorIs(t, err, ErrIdempotencyConflict)`,
    acceptance:
      '新增测试通过；临时从 fingerprintPayload 的构造中漏掉券字段时，该测试应失败，随后恢复实现。fake repo 只证明 service 载荷语义。',
  },
  {
    title: '任务二：追踪库存不足的完整回滚',
    instruction:
      '阅读 internal/order/create_rollback_integration_test.go，写下它如何检查订单、明细、流水、超时事件和券。沿 repository.Create 找到这些写入使用的同一个 db。',
    hint: '关注事务闭包返回 ErrInsufficientStock 后，哪些断言在事务外重新读库。不要只看接口返回了错误。',
    reference:
      '验收清单：无新订单、无新明细、无扣减流水、无超时事件；库存未变；券仍 UNUSED 且未关联订单。真正运行需按 docs/operations.md 配置专用测试库及 TOKMALL_RUN_PG_INTEGRATION=1。',
    acceptance:
      '能解释“写入过”与“已经提交”的区别。未开启真实 PG 时如实记录为阅读完成、集成验证未运行。',
  },
  {
    title: '任务三：画出前端的一次购买尝试',
    instruction:
      '阅读 web/src/routes/product.$id.tsx，找出 key 的创建、保留和重置位置。分别说明网络失败重试与用户主动重新购买时应使用哪个 key。',
    hint: '沿下单 mutation 和重试入口阅读，区分请求没得到响应、已知业务拒绝、开始新购买三种情况。',
    reference:
      '同一次尝试的重试保留 key 与载荷；载荷改变时不能把旧 key 当成更新命令；主动新购才开始新尝试。前端禁用按钮改善体验，服务端锁、指纹和唯一约束负责正确性。',
    acceptance:
      '能指出实际代码位置，并用自己的话讲清 key 的生命周期；发现与预期不一致时先记录现象和复现步骤。',
  },
] as const

export type LessonProgress = {
  explored: string[]
  answers: Record<string, number>
  tasks: number[]
  notes: string
}

export const progressKey = 'tokmall:learn:order:v1'
export function emptyProgress(): LessonProgress {
  return { explored: [], answers: {}, tasks: [], notes: '' }
}

export function parseProgress(raw: string | null): LessonProgress {
  if (!raw) return emptyProgress()
  try {
    const p = JSON.parse(raw)
    if (!p || typeof p !== 'object') return emptyProgress()
    return {
      explored: Array.isArray(p.explored)
        ? experiments.filter((e) => p.explored.includes(e.id)).map((e) => e.id)
        : [],
      answers: Object.fromEntries(
        questions.flatMap((q, i) =>
          Number.isInteger(p.answers?.[i]) &&
          p.answers[i] >= 0 &&
          p.answers[i] < q.options.length
            ? [[String(i), p.answers[i]]]
            : [],
        ),
      ),
      tasks: Array.isArray(p.tasks)
        ? tasks.flatMap((_, i) => (p.tasks.includes(i) ? [i] : []))
        : [],
      notes: typeof p.notes === 'string' ? p.notes.slice(0, 8000) : '',
    }
  } catch {
    return emptyProgress()
  }
}
