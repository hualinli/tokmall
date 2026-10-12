# TokMall 面试问题与练习清单

这份清单用于理解自己的项目，再练习项目面试。组织形式参考 [GoClub 的 GCFeed 面试清单](https://goclub.space/docs/project-study/gcfeed-interview/)；下面的问题、答法与证据按 TokMall 的代码编写。TokMall 使用 Hertz/PostgreSQL/Redis Streams，没有推荐服务、RabbitMQ、推拉混合 inbox 或消息中心，不借用其他项目的实现与成绩。

## 怎么练

回答一题时，依次说明：**当前怎么做 → 为什么这样做 → 哪个代码或实验能证明 → 还有什么限制**。能够脱稿说明、找到证据并回答一个追问后，再勾选该题。复习性能数字时同时说出场景、版本、并发、时长、错误率和业务核对条件。

| 轮次 | 题目 | 先达到什么程度 |
|---|---|---|
| 第一轮：项目与核心链路 | Q01～Q10 | 能演示两个账号完成交易，画出一次下单和一次 Feed 请求，讲清事务、幂等与事件 |
| 第二轮：工程追问 | Q11～Q26 | 能说明失败发生在哪一步、哪些副作用已提交、如何恢复及如何验证 |
| 第三轮：压测与演进 | Q27～Q32 | 能读一份报告、解释等待现场，并区分单轮观察、三轮对照和持续容量 |

业务细则看 [商城业务](commerce-business.md)，锁与事务细节看 [技术设计](commerce-v1-technical-design.md)，取舍看 [ADR](architecture-decisions.md)，具体测量看 [性能证据](performance-baseline.md)。这里保留回答线索，不复制另一份实现状态或性能流水账。

## 第一轮：先把项目讲清楚

### Q01. 项目解决什么问题？怎么用一分钟介绍？

- [ ] 能从用户使用场景介绍项目，再选两个工程亮点展开。

**参考回答：** TokMall 将视频发现与商品交易连接起来：作者发布视频并挂自己的商品，买家从商品卡下单，完成模拟支付、履约或全额退款。后端重点验证并发交易、Feed 缓存分页、异步事件与账实恢复；React 前端用于演示主闭环。

**追问：** 谁是作者、谁是商家？支付是否真实？网页能否用券？同一用户可以承担多个业务身份，资源按 owner ID 授权；支付物流是模拟，券后端已有但网页流程未接。

**证据：** [README 的演示路线](../README.md#第一次演示怎么走)、[业务范围](commerce-business.md)、[前端范围](../web/README.md)。

### Q02. PostgreSQL、Redis、RustFS各负责什么？为什么先做模块化单体？

- [ ] 能画出前端、API、三种依赖和后台任务的关系。

**参考回答：** PG保存订单、库存、状态及Outbox等事实；Redis做缓存、限流、热榜和事件传输；RustFS保存媒体对象。模块分开组织、共享PG，让订单和库存保持本地事务。后台任务与API同进程，当前团队和需求没有要求独立服务部署。

**追问：** Redis停机会不会让PG里的订单消失？拆成微服务会新增什么一致性成本？不能把消息或缓存存储当成交易事实源。

**证据：** [后端架构](backend-architecture.md)、[main装配](../cmd/api/main.go)、ADR-001。

### Q03. 最后一件库存被两个买家同时买，怎么防超卖？

- [ ] 能写出条件扣减，并说明事务内还包含哪些事实。

**参考回答：** 用 `UPDATE inventory SET quantity=quantity-? WHERE sku_id=? AND quantity>=?` 做原子条件扣减，按影响行数判断成功与不足；余额还有DB CHECK。订单、明细、流水、券和超时事件同事务，失败一起回滚。先准备独立写入，库存更新放在事务尾部，成功后直接提交以缩短持锁时间。

**追问：** “先查库存再减”哪里有竞态？提前写流水后库存不足怎么办？准备记录未提交时不会成为对外可读的业务事实。

**证据：** [订单仓储](../internal/order/repository.go)、[500买家争100件](../internal/order/postgres_integration_test.go)、[不足库存全事实回滚](../internal/order/create_rollback_integration_test.go)、ADR-036。

### Q04. 客户端双击或超时重试，如何只生成一单？

- [ ] 能区分同键同载荷重放、同键异载荷冲突和不同键的新请求。

**参考回答：** 幂等范围是 `(user_id,idempotency_key)`，请求指纹包含SKU、数量、来源和券。事务开头按该范围拿 advisory transaction lock，然后查原订单；同载荷返回原单，不同载荷冲突，唯一约束继续兜底。这让券已被首单占用后的同键重试也能先拿到原订单。

**追问：** 为什么只靠订单唯一键不够保证“同键同券”的重放体验？旧实现先锁券，后来的请求会先遇到USED而失败。

**证据：** [Create实现](../internal/order/repository.go)、[同键同券并发测试](../internal/order/coupon_postgres_integration_test.go)、[技术设计](commerce-v1-technical-design.md#创建与幂等)。

### Q05. 支付、取消、发货和退款发生竞争时，状态怎么守住？

- [ ] 能画出订单主状态，并区分履约与退款的允许迁移。

**参考回答：** 支付、取消、超时使用订单行锁、状态条件与流水唯一键。履约命令有独立迁移表，不能复用含退款恢复边的完整状态图。退款仅支持PAID/FULFILLING；拒绝恢复原状态，同意全额退款，未发货回补库存、已发货不回补，COMPLETED不接受退款。

**追问：** 为什么退款申请期间不能发货？曾复用完整状态图而允许错误履约，数据库约束和测试暴露问题；退款恢复边不应成为发货命令的许可。

**证据：** [订单状态机](../internal/order/service.go)、[退款实现](../internal/order/refund_repository.go)、[真实退款竞争测试](../internal/order/refund_postgres_integration_test.go)。

### Q06. 一次Feed请求经过哪些步骤？为什么缓存命中还会访问数据库？

- [ ] 能从路由讲到页面读取、动态装配与响应，不只说“用了Redis”。

**参考回答：** Handler解析参数，Service校验游标并从缓存或仓储拿视频页，按需要复制共享结果，再读取点赞与商品卡、签发相关URL并返回。视频主体可以缓存；点赞、商品关系、商品状态和最低价按请求合并成一次SQL读取，以保留动态新鲜度。

**追问：** 多个商品和SKU一起JOIN会不会把点赞数乘大？点赞和SKU价格先分别聚合再连接。

**证据：** [Feed服务](../internal/feed/service.go)、[合并读模型](../internal/feed/dynamic_read.go)、[计数/新鲜度真实PG测试](../internal/feed/dynamic_read_integration_test.go)。

### Q07. 为什么用游标分页？时间相同的视频怎么办？

- [ ] 能解释排序键、上一页末项、limit+1和游标签名。

**参考回答：** 时间流按 `(published_at,id)` 倒序，有相同时间时用ID形成稳定全序；下一页查询小于上一页末项的记录。多取一条判断has_more。游标带排序版本并由HMAC签名，Base64只是传输编码，不提供防篡改能力。

**追问：** 新视频插到首页是否改变旧游标边界？视频删除或可见性变化是否意味着整个浏览过程是数据库快照？时间游标提供稳定边界，不冻结全站内容。

**证据：** [游标](../internal/feed/cursor.go)、[SQL](../internal/feed/repository.go)、[关注时间线集成测试](../internal/feed/postgres_integration_test.go)、ADR-004。

### Q08. Cache-Aside、singleflight和L1/L2各解决什么问题？

- [ ] 能说明缓存命中、失效、并发回源和共享结果的归属。

**参考回答：** Cache-Aside在miss时回源并回填；singleflight合并进程内同key并发回源，执行前再检查缓存；L1本地LRU减少Redis读取，L2用于共享缓存。回源使用独立超时上下文，避免首个请求取消扩散给其他等待者；装配动态字段前复制共享结果。

**追问：** singleflight能否跨进程合并？发布后远端L1怎样失效？删L2、删本地L1并递增版本，远端定期探测；失败路径依赖TTL兜底，不保证强一致或故障下的固定新鲜度上界。

**证据：** [服务缓存逻辑](../internal/feed/service.go)、[两级缓存](../internal/feed/tiered_cache.go)、[测试](../internal/feed/tiered_cache_test.go)、ADR-025。

### Q09. 支付提交后进程崩溃，销售统计怎么办？为什么用Outbox？

- [ ] 能指出“提交事实”和“发送外部消息”之间的故障窗口。

**参考回答：** 在同一PG事务写支付事实和Outbox。中继之后发送Streams，发送成功再标记发布，避免事实已提交却没有持久化事件意图。发送成功但标记前崩溃仍可能重发，所以投递是至少一次。

**追问：** 为什么在PG事务里直接发Redis消息也不等于原子提交？PG回滚不能撤销外部发送，两个系统没有共同的本地事务。

**证据：** [Outbox Enqueue](../internal/outbox/model.go)、[中继](../internal/outbox/relay.go)、[真实依赖测试](../internal/outbox/integration_test.go)、ADR-014/016。

### Q10. 同一消息收到两次，怎么避免统计重复加？什么时候ACK？

- [ ] 能讲清回执、投影、提交与XACK的先后顺序。

**参考回答：** 消费事务先按 `(consumer,event_id)` 记录唯一回执并执行投影，二者一起提交，再XACK。回执冲突表示该消费者已处理，不重复生效。提交后ACK前崩溃会再次收到消息，但不会再次累计。没有回执表示正常应用路径的消费事务没有生效。

**追问：** 为什么不能人工删回执再重放？若投影仍在，去重依据被删除会造成重复累计。正确恢复依靠缺失回执条件或从事实重建读模型。

**证据：** [消费者](../internal/outbox/consumer.go)、[投影](../internal/attribution/projector.go)、[缺失回执测试](../internal/reconcile/missing_receipts_test.go)。

## 第二轮：解释异常与取舍

### Q11. Handler、Service、Repository怎么分工，错误如何传递？

- [ ] 能沿一个订单请求指出三层代码和一个错误映射。

**参考回答：** Handler做HTTP解析、认证身份读取与错误响应；Service做业务规则和用例编排；Repository做SQL/事务并将ORM错误翻译为领域错误。service返回领域视图，model不作为跨层或跨模块API。包装用 `%w`，分类用 `errors.Is/As`，业务拒绝不按系统故障记录。

**追问：** 模块间如何合作而不互相导入业务model？使用消费侧接口和本模块read model，由main装配。账号status零值曾绕过DB默认值导致注册后无法登录，说明ORM写入与数据库默认值也要验证。

**证据：** [订单Handler](../internal/order/handler.go)、[Service](../internal/order/service.go)、[仓储](../internal/order/repository.go)、[注册后登录真实PG测试](../internal/user/postgres_integration_test.go)。

### Q12. 订单过期后收到支付请求，为什么不能在事务内直接返回过期错误？

- [ ] 能区分“业务请求失败”与“必须提交的关单副作用”。

**参考回答：** 支付路径做lazy expiry，写EXPIRED、回补库存与释放券。这些副作用要提交；事务闭包直接返回错误会一起回滚。实现先提交过期处理，再在事务外返回订单已过期。延迟事件加速关闭，轮询worker提供安全网。

**追问：** 三条超时路径同时到达会不会重复回补？订单行锁、状态条件和账本唯一键共同守住一次生效。

**证据：** [支付/过期仓储](../internal/order/repository.go)、[超时处理](../internal/order/timeout_handler.go)、[竞争测试](../internal/order/postgres_integration_test.go)、ADR-021。

### Q13. 券怎么防超发、重复使用和关单后无法重用？

- [ ] 能说明发券/领券/下单核销的锁与唯一约束，以及金额规则。

**参考回答：** 领取锁模板、条件更新发行量并检查用户限领；下单锁持券，检查归属、同商家、有效期和门槛，以集中规则计算折扣。订单满足 `original-discount=total` 且实付大于零。取消、超时和退款成功退券；历史订单保留引用，部分唯一索引只限制仍占券订单。

**追问：** 过期券退回什么状态？能否叠券或跨商家？退为EXPIRED；当前只有同商家满减，一单一券。

**证据：** [券规则](../internal/coupon/service.go)、[领取竞争测试](../internal/coupon/postgres_integration_test.go)、[用券/关单测试](../internal/order/coupon_postgres_integration_test.go)、[迁移000024](../migrations/000024_closed_order_coupon.up.sql)。

### Q14. 热榜如何保持翻页稳定？Redis坏了怎么处理？

- [ ] 能区分热榜快照与时间流，也能说明首页和后续页的降级差异。

**参考回答：** 点赞/播放写分钟桶，后台物化冻结v2窗口，最多2000项、TTL10分钟。游标包含窗口、分数和ID；窗口不重写，到期拒绝旧游标。空首页或首页Redis故障可降级时间流；返回时间游标后继续时间流。已有热榜游标遇故障返回错误，避免途中换排序。

**追问：** 包里的HotRanker能直接用于关注分页吗？它是实验路径，生产Service只接受TimeRanker。播放event_id幂等也不等于有效播放防刷。

**证据：** [热榜](../internal/feed/hot_cache.go)、[服务/分页测试](../internal/feed/hot_service_test.go)、[Ranker测试](../internal/feed/ranker_test.go)、ADR-027。

### Q15. 关注Feed是推还是拉？为什么不只取每个作者最近五条？

- [ ] 能说明当前查询策略和未来推拉混合的进入条件。

**参考回答：** 当前拉模式读完整关注时间线；无关注短路，小集合按作者有界召回再归并，大集合走全局时间索引过滤。每作者固定TOP5会永久丢掉更旧作品，不符合完整时间线。用户第一页缓存按关注关系失效，发布/删除不遍历全部粉丝key，靠TTL收敛。

**追问：** 大V和大量关注时怎样扩展？先测读取成本，再评估推送inbox或混合模式；当前没有交付fanout。

**证据：** [关注SQL](../internal/feed/repository.go)、[完整时间线测试](../internal/feed/postgres_integration_test.go)、[查询计划脚本](../scripts/explain_following.sql)。

### Q16. 点赞、关注、播放的幂等有什么差别？

- [ ] 能说明关系存在性与事件载荷一致性的不同约束。

**参考回答：** 点赞/关注由唯一关系配合ON CONFLICT避免重复关系。播放同步落PG，event_id必须绑定一致载荷；重放不重复计数，同ID换视频或其他事实返回冲突。每个新event_id仍可产生新播放，因此未实现同用户同视频时间窗口的有效播放去重。

**追问：** 幂等能否防一个用户刷热榜？不能把重试去重当反作弊；有效播放口径尚待定义。

**证据：** [互动仓储](../internal/interaction/repository.go)、[播放仓储与Service](../internal/video/repository.go)、[后续计划](roadmap.md)。

### Q17. 如何处理大量不存在的视频ID？Bloom会不会误伤合法请求？

- [ ] 能区分真实不存在、未发布不可见和过滤器尚未加载。

**参考回答：** Bloom只在确定不可能存在时短路，首次加载前放行，重建双缓冲轮换；负缓存只存DB真实miss。Bloom可能误判“可能存在”，这时继续查；未发布但不可见的视频不能被当成不存在写共享负缓存。

**追问：** 新发布内容还没进入过滤器怎么办？结合增量更新、warmup和重建策略核对，不能让未初始化过滤器直接拒绝全部ID。

**证据：** [存在性防护](../internal/existence/)、[视频读路径](../internal/video/existence.go)、ADR-022。

### Q18. 为什么媒体先上传staging，再复制到最终对象？

- [ ] 能说明签名、票证、对象大小检查和ETag条件封存。

**参考回答：** 客户端只获得临时key的预签名PUT，签名绑定声明长度/类型；封存时校验实际大小并按源ETag复制最终key，避免检查后被替换。票证归属、到期和核销受PG事务与行锁保护，视频创建与票证核销同事务。

**追问：** RustFS复制成功但PG提交失败会怎样？外部副作用不能回滚，可能留下未知final孤儿；当前没有扫描回收这类对象的完整机制。

**证据：** [上传服务](../internal/video/service.go)、[存储契约测试](../internal/storage/upload_integration_test.go)、ADR-029。

### Q19. 文件已经上传，为什么还要异步校验后才发布？

- [ ] 能区分PENDING、PASSED、REJECTED与SKIPPED。

**参考回答：** 对象存在与大小合法不代表媒体可播放。worker下载后用ffprobe检查媒体策略，必要时ffmpeg处理faststart。坏文件拒绝；工具缺失/关闭探测记SKIPPED；暂时下载或探测失败延迟重试。PASSED/SKIPPED才可发布，服务层和DB都守门，前端只展示状态和安全原因。

**追问：** SKIPPED能不能说成媒体通过验证？不能。多worker可能重复探测，但回写不能复活已删除视频。

**证据：** [探测worker](../internal/video/probe_worker.go)、[PG门禁测试](../internal/video/probe_postgres_integration_test.go)、[媒体工具测试](../internal/mediaprobe/probe_test.go)、ADR-028。

### Q20. 删视频时对象存储不可用，如何保留删除意图？

- [ ] 能说明逻辑删除、持久化任务与物理删除之间的关系。

**参考回答：** 视频删除状态与object_delete_jobs同事务，后台重试物理删除。staging清理要等原PUT到期，防止旧签名再写入；媒体结果回写持视频行锁，不复活删除对象。旧GET链接在对象物理删除前仍可能可访问。

**追问：** 任务队列是否能回收所有未知孤儿？是否有最大重试和专用告警？当前边界分别见架构与roadmap，不把删除补偿扩展成完整配额/孤儿治理。

**证据：** [清理worker](../internal/video/cleanup_worker.go)、[清理竞争测试](../internal/video/cleanup_postgres_integration_test.go)、[迁移000023](../migrations/000023_object_cleanup.up.sql)。

### Q21. JWT如何续期、撤销与防越权？

- [ ] 能区分身份认证、资源归属校验与refresh一次性使用权。

**参考回答：** Access/refresh分工，refresh轮换用Redis原子Claim阻止同token并发双签发。资源操作按认证上下文中的用户ID验证owner，不能信任客户端user_id；不存在与不可见资源按相同404隐藏。生产要求显式强密钥与TLS。

**追问：** access token是否逐请求检查账号停用？localStorage token能否防XSS？当前没有即时账户撤销检查或HttpOnly Cookie改造，不把JWT当完整权限系统。

**证据：** [认证中间件](../internal/middleware/auth.go)、[refresh Claim](../internal/user/blacklist.go)、[前端认证](../web/README.md)、ADR-018/020。

### Q22. 限流如何取用户/IP/账号key？怎样防伪造和超长key？

- [ ] 能说明身份来源、可信代理和账号规范化/哈希。

**参考回答：** Redis固定窗口Lua限制读IP、写用户及认证请求。默认不信任转发头；账号规范化后SHA-256，认证体积另有限制。登录/注册检查账号配额，refresh/logout只走IP配额，避免NAT用户共用账号桶。限流Redis故障fail-open并记录日志。

**追问：** 用户名与邮箱是否共用同账号桶？第三方是否能消耗受害者账号配额？当前按标识符分桶且有账号拒绝服务取舍，尚非按账号累计失败的退避体系。

**证据：** [限流](../internal/middleware/ratelimit.go)、[配置](../internal/config/config.go)、[测试](../internal/middleware/ratelimit_test.go)、ADR-032。

### Q23. 视频成交来源可信到什么程度？毛额和退款如何统计？

- [ ] 能区分来源关系验证、订单快照、实时统计与异步日销。

**参考回答：** 只允许作者挂自己的在售商品；下单验证活动关系并保存来源和价格快照，不证明真实点击，也未重新验证视频发布状态。作者接口以同一只读快照聚合订单事实；日销另由事件投影。毛额保留所有曾支付订单，成功退款按发生日单列；申请退款不减毛额。

**追问：** 日销按哪个时区？当前按UTC自然日，不自行说成北京时间。归因是否能直接用于佣金结算？目前不支持可信点击归因或结算。

**证据：** [实时统计](../internal/attribution/repository.go)、[日销投影](../internal/attribution/projector.go)、[来源校验](../internal/order/repository.go)、ADR-017。

### Q24. 为什么消费者pending/lag为0，业务仍可能缺事件？

- [ ] 能通过消息身份解释裁剪故障，说明正式修复与诊断对照的差别。

**参考回答：** 原长度裁剪会删除未消费消息。诊断记录PG event_id与成功XADD的Stream ID，确认缺失事件都早于保留边界且不在Stream/DLQ中；只提高上限后正常消费能完成，是根因对照。正式发布按所有消费组确认水位安全回收，容量满时背压，未投递事实留在PG，不靠增大固定缓冲保证不丢。

**追问：** 能否声称积压永远不增长？不能；消费不足仍要提速或入口保护。已到期PENDING与未来timeout要分开，库内DEAD与消费DLQ也不同。

**证据：** [完整ID诊断](../benchmarks/2026-10-11/diagnosis/README.md)、[传输Lua](../internal/outbox/transport.go)、[多组/乱序ACK/背压测试](../internal/outbox/retention_integration_test.go)、ADR-033。

### Q25. 对账发现差异后，能自动修复哪些，不能修复哪些？

- [ ] 能说出九项检查的类别与重放/重建的范围。

**参考回答：** 九项覆盖库存/订单流水、支付/退款一致、券发行和使用、订单金额拆分、日销投影及事件回执。对账报告差异，缺失回执事件可按原ID重放，日销读模型可从事实重建；它不自动改订单、资金、库存和券等事实。

**追问：** 为什么不能直接“把差异改成0”？先验证事实可信，按差异类别恢复；不能删已消费回执来制造重放条件。

**证据：** [九项SQL](../internal/reconcile/repository.go)、[差异/恢复集成测试](../internal/reconcile/integration_test.go)、[CLI与运维](development-and-operations.md#观测与恢复)。

### Q26. 日志、指标、traceparent分别回答什么问题？

- [ ] 能说明请求关联、指标标签和项目的追踪边界。

**参考回答：** slogContext日志关联request/trace/route/user并脱敏；Prometheus指标观察请求延迟、池等待和队列；traceparent校验/生成/透传帮助关联请求。路由标签用模板，避免user_id/order_no高基数。已有规则与本地看板，没有完整OTel exporter和Alertmanager通知链路。

**追问：** `/health/ready`通过是否代表全部依赖正常？目前只检查PG，RustFS不可用不阻塞启动。对象不存在是业务结果，不应触发存储熔断失败累计。

**证据：** [观测模块](../internal/observability/)、[健康检查](../internal/health/)、[存储熔断](../internal/breaker/storage.go)、[运维](development-and-operations.md)。

## 第三轮：把测量和排障讲到证据

### Q27. 怎样设计一次可信的压测？并发、RPS、订单TPS有什么区别？

- [ ] 能读一轮报告并说出实验条件，而不是只记吞吐。

**参考回答：** 用k6固定VU模型，准备阶段与测量分开，独立PG库/专用空Redis库，记录版本、二进制SHA、环境、缓存与限流/worker设置。Feed检查20项，交易一次迭代下单再模拟支付；完整成功订单才计TPS，不能拿两次HTTP请求的RPS当订单TPS。反复测相同档位，报告各轮与中位数、P95/P99、错误率。

**追问：** 固定VU是否等于固定每秒流量？服务变慢时下一次迭代也变慢，尚不能证明持续到达率下积压稳定。HTTP成功后还要核对库存、支付、回执、投影和九项对账。

**证据：** [k6场景](../scripts/k6-baseline.js)、[调度脚本](../scripts/benchmark_k6.py)、[压测指南](development-and-operations.md#k6-benchmarks)。

### Q28. ORM把SQL藏起来后，怎样在压测现场查行锁竞争？

- [ ] 能解释实时采样的SQL和观察字段，并找到一个等待关系。

**参考回答：** 压测同时采样pg_stat_activity的SQL、state、wait_event_type/wait_event与pg_blocking_pids。单热点场景中等待集中在库存更新；结合应用池in_use饱和、WaitCount/WaitDuration增量和Go block profile的DB.conn路径，建立“库存锁等待占连接→共享池排队→消费者受挤压”的证据链。

**追问：** 当前SQL相同是否证明同一行？混合流量还要核对参数；单SKU受控场景已知同一目标。block占比是累计goroutine阻塞时间，不是CPU比例。

**证据：** [动态锁等待命令](development-and-operations.md#lock-waits)、[诊断采样](../benchmarks/2026-10-11/diagnosis/README.md)、[连接池指标](../internal/observability/dbpool.go)。

### Q29. 为什么EXPLAIN很快，接口仍然慢？pprof应该选哪种？

- [ ] 能区分连接获取、数据库执行、网络往返和应用处理。

**参考回答：** 从ORM拿到实际SQL和代表性参数，用EXPLAIN ANALYZE看执行计划、扫描/聚合和执行成本。它不包含应用获取连接的排队。CPU profile看CPU开销；block看goroutine阻塞；mutex看Go锁竞争；数据库锁仍由PG等待现场解释。

**追问：** 本项目pprof端口是否默认开启？当时只用诊断overlay监听回环19092并开启采样，普通API不自动提供该端点。可直接读已归档profile练习。

**证据：** [profile命令](development-and-operations.md#profiles)、[执行计划与profile归档](../benchmarks/2026-10-11/diagnosis/)、[Feed SQL](../internal/feed/dynamic_read.go)。

### Q30. 做了什么优化，如何排除“只是改配置或冻结数据”的收益？

- [ ] 能分别说出优化变量、正确性约束、对照与测量限制。

**参考回答：** 安全裁剪/背压先解决缺口；独立消费预算保持总上限25；Feed合并两次动态读取并用真实PG测试保证新鲜度；库存更新移至事务尾部而不拆事务。同预算Feed双方各60秒×3，吞吐中位提高约43%；热点库存优化前只有单轮参考，之后三轮，不能说成严格三轮A/B。

**追问：** 诊断冻结字段的更高吞吐能不能写成果？不能，该变体不保证写后更新。分散SKU波动范围包含参考，不主张显著收益；停压后仍需消费收敛，尚未建立长期容量。

**证据：** [最近分步结果](performance-baseline.md#latest-results)、[Feed对照](../benchmarks/2026-10-12/step3-feed-dynamic/README.md)、[库存对照与验证限制](../benchmarks/2026-10-12/step4-stock-lock/README.md)、ADR-034～036。

### Q31. 单元测试、真实依赖测试、契约与E2E分别证明什么？

- [ ] 能说明每类测试的覆盖，并确认真实依赖开关已启用。

**参考回答：** 单元测试检查领域规则与分支；真实PG验证锁/约束/事务竞争，Redis/RustFS测试验证对应协议语义；OpenAPI测试核对路由和DTO契约；浏览器E2E检查用户操作链。默认Go测试跳过真实依赖，模拟媒体E2E也不等于真实ffprobe校验。

**追问：** 是否所有最新全套检查都通过？10月12日改动相关真实PG/Redis回归与lint通过，最后全量补跑遇宿主启动卡顿，未记为成功；完整迁移回滚和当前所有场景容量也未覆盖。

**证据：** [测试矩阵](development-and-operations.md#测试矩阵)、[CI](../.github/workflows/ci.yml)、[最后一轮验证范围](../benchmarks/2026-10-12/step4-stock-lock/verification.md)。

### Q32. 流量再增长，什么时候考虑MQ、微服务或分库？

- [ ] 能从已测瓶颈提出下一步，说明新增方案的成本。

**参考回答：** 当前先评估热点进入DB前的有界并发保护、批量消费、固定到达率和读写混合，确认资源和积压是否稳定。新增broker不能自动消除消费端的DB瓶颈；拆服务但仍访问同库不会消除库存行竞争。分片能分散不同数据负载，同一SKU仍需有明确库存一致性方案。

**追问：** 哪些是未来设想，哪些已经交付？Kafka、真实支付物流、完整权限/治理、推拉混合、批量消费和热点入口保护都不能说成当前已实现。

**证据：** [roadmap](roadmap.md)、[架构边界](backend-architecture.md#明确边界)、[性能证据](performance-baseline.md)。

## 动手练习：一次只完成一件事

| 顺序 | 练习 | 完成标准 |
|---|---|---|
| 1 | 按README用两个账号演示一次交易，再用另一单演示退款 | 能解释每次状态变化、库存何时扣减/回补；不把模拟支付当渠道回调 |
| 2 | 阅读订单Create、Pay与一个真实PG并发测试 | 能画出事务范围，找出条件库存更新、幂等锁与影响行数判断 |
| 3 | 查看一轮summary.json和reconcile.json | 能区分请求/交易计数，核对支付、库存、流水、回执和投影 |
| 4 | 离线打开block profile及trim-proof.json | 能指出DB.conn等待路径，并说明成功投递与安全保留不是同一个事实 |
| 5 | 在专用环境运行一轮压力，同时采样PG等待 | 能读出等待者/阻塞者/SQL，区分瞬时快照与连续采样 |
| 6 | 复跑同条件对照，练习两分钟讲解 | 每个数字都带测量条件；同时说明正确性约束和未解决的消费积压 |

命令统一维护在 [开发与运维](development-and-operations.md#k6-benchmarks)。原始实验目录的README记录版本、参数和限制；不要在有业务数据的库中运行造数/清理实验。
