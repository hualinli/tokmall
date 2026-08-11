# Commit Message 速查表（Conventional Commits）

> 用法：`git commit -m "type: 动词原形 + 改了什么"`，需要说明原因时再加 `-m "正文"`。

## 模板

```
type: add/fix + 名词短语        ← 标题（对代码的命令，动词用原形）
                                ← 空行
为什么这么改（可选）             ← 正文，短语即可，不需要完整句
```

标题长度建议不超过 50 字符；正文每行不超过 72 字符。

## type 速查（判断标准：这次改动会不会改变程序行为？）

| type     | 什么时候用                       | 例句 |
|----------|--------------------------------|------|
| `feat`   | 新功能                           | `feat: add jwt auth middleware` |
| `fix`    | 修 bug                          | `fix: return 404 for missing video` |
| `docs`   | 改文档                           | `docs: explain cursor pagination` |
| `refactor` | 重构，行为不变                 | `refactor: extract handler into service layer` |
| `test`   | 加/改测试                        | `test: cover request id passthrough` |
| `chore`  | 杂务（依赖、配置、工具）          | `chore: add lumberjack dependency` |
| `perf`   | 性能优化                         | `perf: cache feed first page` |
| `style`  | 格式，无逻辑变化                 | `style: gofmt` |

判据：

- 行为**变了** → `feat`（新增）或 `fix`（修复）
- 行为**没变** → `refactor` / `test` / `style` / `chore`
- 一个 commit 只做一件事；多个主题拆多个 commit，便于单独 revert

## 正文怎么写（英语不好也能写）

正文是"为什么"，用短语/残缺句完全合法：

```
feat: add requestid middleware

Use uuid v7 because it is time-ordered and friendly to db index.
```

常用句式：

- `Use X because ...`
- `X so that ...`
- `for ...`
- `Fixes #123` / `Closes #456`（关联 issue，GitHub 会自动关）

## 动词原形对照（不要用过去式）

| 对   | 错          |
|------|-------------|
| add  | added / adding |
| fix  | fixed / fixing |
| remove | removed |
| update | updated |
| move  | moved   |
| rename | renamed |
| bump  | bumped  |

## 示例（本项目）

```
feat: add recovery middleware
feat: add requestid middleware
feat: add structured logging
chore: ignore logs directory
```

## 什么时候必须写正文

- 一个 commit 包含多个逻辑改动时（记录每个改动的理由）
- 决策有取舍时（记录"为什么这样选"，三个月后自己也能看懂）
- 单文件、单意图的小改动，标题足够，正文可省
