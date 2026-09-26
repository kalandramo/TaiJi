# Taiji · 命令系统与 RBAC 需求文档

> 项目：**taiji**（Go 原生 Agent Harness 原型）
> 版本：v1.0 · 2026-09-24
> 前置：`01-需求文档.md` FR-10（IM 渠道接入与权限管控）、`07-RBAC权限设计.md`（RBAC 接入设计）
> 参照系：`happyclaw/src/commands.ts`、`happyclaw/src/im-command-utils.ts`、`apache/casbin`

---

## 0. 结论摘要

本需求回答两个问题：

| 问题 | 结论 | 现状 |
|---|---|---|
| **Q1 飞书命令系统** | 需从零建设（当前 **0 实现**） | `internal/` 全仓无命令解析代码 |
| **Q2 RBAC 与依赖策略** | RBAC 已自研落地；casbin/DB 是**演进选项**，非当下必需 | `internal/authz/rbac.go` 200 行已实现 |

**最重要的一条判断**：用户提出「引入 casbin、数据库」，但**当前 RBAC 已用 200 行自研实现且测试完备**。
引入 casbin 的收益是「不用自己维护继承/防环/校验逻辑」，代价是**新增一个依赖 + 一套 model.conf 心智模型**。

> 依据用户「优先基于已有开源生态实现，后续慢慢减少依赖」的指示，
> 本文档把 casbin 定位为**可替换实现（方案 B）**，与现有自研实现（方案 A）
> 通过同一个 `PermissionSource` 接口并存——**换实现不改消费方**。
> 这不是「现在必须做」，而是「留好接缝，需要时能换」。见 §3。

---

## 1. 定位：Taiji 是「基于 LLM 的操作系统」

用户明确 TaiJi 的定位是**基于 LLM 的操作系统**。这个定位不是修辞——它直接决定了
权限模型必须比「一个聊天机器人」严格得多：

| 类比 | 对应物 | 权限含义 |
|---|---|---|
| 内核 | Agent Loop（`chat.Run` / `Executor`） | 唯一可信执行体 |
| 系统调用 | 工具（MCP tools） | 必须逐次鉴权 |
| **用户/进程** | Principal（IM 用户） | 必须可识别、可授权 |
| **文件/资源** | 工作区（workspace） | 必须有归属（owner） |
| **root/sudo** | admin 角色 | **不自动绕过资源所有权**（FR-10.6） |

**操作系统类比推出的硬约束**：

1. **默认拒绝**（fail-closed）——不是「默认允许再收紧」。已落地（`toolpolicy.go`）。
2. **身份不可伪造**——主体从平台元数据实取，不信任调用方传入（FR-10.2）。
3. **权限判定与执行分离**——判定在插件层，执行在内核层（`permission_plugin.go:52`）。
4. **一切拒绝留痕**——审计是操作系统级要求，不是可选功能（FR-10.9）。

> **已同步（2026-09-24）**：本文档初稿曾标出「`01-需求文档.md` 的定位是『生产级 Go agent harness』，
> 与本文档的『基于 LLM 的操作系统』不一致」这一张力。**用户已拍板同步**，`01` 现已改为两层表述：
>
> - **产品定位**：基于 LLM 的操作系统（新增，G7 给出四条权限硬约束）
> - **实现形态**：Go 原生 agent harness（保留）
>
> `01` 同步内容：标题、定位行、核心问题表述、新增 G7（操作系统级权限严谨性）、
> 新增 N6（「操作系统」是类比不是实现目标），版本 v1.4.1 → v1.4.2。
>
> **注意 `harness` 一词的处理**：`01` 正文中 `harness` 出现 30 次，
> 但绝大多数是**引用他项目**（`Tianshu-harness`、`deepseek-harness`）或**实现形态术语**
> （「Go 原生 harness」）。产品定位与实现形态是两个层次，**不做全局替换**——
> 只改定位表述，保留实现形态术语。这是有意区分，不是遗漏。

---

## 2. Q1：飞书命令系统（从零建设）

### 2.1 现状（实测）

**命令系统完全不存在**。证据：

```
$ grep -rn "CommandHandler|parseCommand|isCommand|/stop|/help" --include=*.go internal/ cmd/
（零命中）

$ grep -n 'switch args\[0\]' cmd/taiji/main.go
67:	switch args[0] {
71:	case "chat":
73:	case "serve":
```

CLI 只有 `chat` / `serve` 两个子命令（`cmd/taiji/main.go:67-73`），
**IM 侧没有任何命令解析**。用户发的 `/help` 会被当作普通消息送进模型。

**唯一的命令痕迹是需求，不是实现**：`01-需求文档.md:590` 提到
「控制类命令（如 `/stop`）必须豁免限流」——但 `/stop` 本身不存在。

### 2.2 需求目标

#### FR-C1 命令识别与拦截

- 命令必须**在进入模型前被拦截**，不得作为普通消息送入 LLM。
  - 对齐 happyclaw `src/commands.ts:2` 的原文：「intercepts text commands (e.g. /clear) **before they enter the normal message pipeline**」。
- 识别规则：消息正文以 `/` 开头且首个 token 匹配已注册命令名。
- **非命令的 `/` 开头文本不得误伤**（如用户发 `/usr/local/bin 是什么`）——
  未注册的命令名应走正常消息路径，而不是报错。

#### FR-C2 命令清单（原型最小集）

| 命令 | 作用 | 权限要求 | 依据 |
|---|---|---|---|
| `/help` | **列出全部可用命令**（用户显式要求） | 任何通过门禁的用户 | 本文档 |
| `/status` | 查看当前会话/队列状态 | 任何通过门禁的用户 | happyclaw `formatSystemStatus` |
| `/clear` | 清除当前会话上下文 | **owner** | happyclaw `OWNER_REQUIRED_IM_COMMANDS` |
| `/stop` | 中断当前生成 | **owner** | `01-需求文档.md:590` 已承诺豁免限流 |
| `/owner_mention` | 认领群 owner | 任何成员（**唯一引导路径**） | FR-10.5 |
| `/release_owner` | 释放 owner | **owner** | FR-10.5 |

> **`/help` 是本次用户显式要求的核心命令**——「支持列出 taiji 命令，命令必须正常使用」。
> 它的输出必须**从命令注册表动态生成**，不是硬编码字符串——否则新增命令必然忘记更新帮助。

#### FR-C3 命令的权限判定（与 RBAC 的交汇点）

这是命令系统与 RBAC 的**接缝**，也是最容易做错的地方。

happyclaw 的做法（`im-command-utils.ts:272`）是一张**静态集合**：

```typescript
export const OWNER_REQUIRED_IM_COMMANDS: ReadonlySet<string> = new Set([
  'clear', 'fresh', 'bind', 'unbind', 'new', 'sw', 'spawn', 'release_owner',
]);
```

**本设计的改进（有意分歧）**：不另建集合，而是**复用 RBAC 的权限点机制**。

```
命令权限点：cmd:help, cmd:status, cmd:clear, cmd:stop, cmd:owner_mention, cmd:release_owner

Role: viewer    → cmd:help, cmd:status
Role: operator  → viewer 权限 + cmd:clear, cmd:stop
Role: admin     → operator 权限 + cmd:release_owner
```

**理由（三条）**：

1. **一致性**：FR-10.6 要求「同一动作在四个面权限一致」。命令是第五个面——
   若命令走独立集合，就制造了第二套权限真相。
2. **可配置**：静态集合意味着「谁能用 /clear」写死在代码里。权限点让它可配。
3. **复用已实现的继承**：`rbac.go` 的 RBAC1 继承（`collectPermissions`）自动生效——
   admin 不需要重复声明 operator 的命令权限。

**代价（明说）**：权限点空间从「工具名」扩展到「命令名」，
`matchToolPattern`（`permission.go:127`）的前缀通配需确认对 `cmd:` 前缀无歧义
（当前工具名不含 `:`，故 `cmd:*` 通配安全）。

#### FR-C4 命令必须「正常使用」（用户原话）

「命令必须正常使用」是一条**验收级**要求，拆为四条可测条件：

1. **可达**：命令在任何通过门禁的会话里都能触发（私聊 + 群聊）。
2. **有响应**：命令必有回复，且**不调用模型**（省 token + 确定性响应）。
3. **权限正确**：无权限用户执行受限命令 → 收到**明确拒绝**（复用 `denyMessage` 的文案策略，
   `permission_plugin.go:128`），而非静默忽略。
4. **不污染上下文**：命令**不写入会话历史**——否则 `/help` 会变成模型的上下文噪声，
   且占用 token。

> 第 4 条依据 happyclaw 的实现：命令在 `storeMessage` 之前拦截，不入库。

### 2.3 非目标（明确不做）

- **不做飞书原生指令菜单**（bot 菜单配置）——那是平台侧配置，不是代码需求。
  若需要，应作为独立 issue（涉及开放平台后台操作，无法用代码验证）。
- **不做命令别名**（`/h` → `/help`）——v1 保持一命令一名，避免别名表成为第二套真相。
- **不做命令参数解析框架**——v1 只做「命令名 + 剩余文本」两级
  （对齐 happyclaw `parseFreshCommand` 的 `/^\/fresh(?:\s+([\s\S]*))?$/`）。
- **不做多语言**——文案中文，与现有 `denyMessage` 一致。

---

## 3. Q2：RBAC 与依赖策略

### 3.1 现状：RBAC 已自研实现（实测）

**这是本需求最重要的现状事实**——用户在提「引入 casbin」，但 RBAC **已经做了**：

```
$ ls internal/authz/
context.go  context_guard.go  permission.go  permission_plugin.go
principal.go  rbac.go  toolpolicy.go

$ wc -l internal/authz/rbac.go
200 internal/authz/rbac.go
```

已实现的能力（`rbac.go`）：

| 能力 | 实现 | 锚点 |
|---|---|---|
| `User → Role → Permission` | `RBACPermissions` | `rbac.go:42` |
| RBAC1 角色继承 | BFS 展开 + visited 防环 | `rbac.go:78` `collectPermissions` |
| 配置校验 | 引用未定义角色即报错 | `rbac.go:129` `Validate` |
| 通配匹配 | 复用 `matchToolPattern` | `permission.go:127` |
| fail-closed | 未绑定用户拒绝 | `rbac.go:170` |

**装配**：`TAIJI_RBAC` 环境变量（`role:` / `parent:` / `user:` 三类前缀），
`resolvePermissions()` 统一入口，**RBAC 优先，未配时回退 `TAIJI_USER_PERMISSIONS`**。

**测试**：`rbac_test.go` 13 例 + `rbac_validate_test.go` + `cmd/taiji/rbac_test.go` 6 例，
含反证验证（禁用继承 → 继承测试变红）。

### 3.2 依赖策略：三段式演进

用户指示：「可以引入 casbin，数据库等依赖，**优先基于已有开源生态实现，后续慢慢减少依赖**」。

这句话包含一个**内在张力**，必须点明：

> 「优先用开源生态」与「后续慢慢减少依赖」方向相反——
> 前者增加依赖，后者减少依赖。

**我的解读**（若理解有偏差请纠正）：真实意图是**先用成熟实现快速跑通，把边界稳定下来，
再按需自研替换**——即「**依赖是脚手架，不是地基**」。

据此设计三段式：

```mermaid
flowchart LR
    A["阶段一（当前）<br/>自研 RBAC 200 行<br/>零外部依赖"] --> B["阶段二<br/>引入 casbin<br/>换 PermissionSource 实现"]
    B --> C["阶段三<br/>按需替换<br/>保留接口，换实现"]

    style A fill:#2d4a3a,stroke:#4a7,color:#fff
    style B fill:#4a3a2d,stroke:#a74,color:#fff
    style C fill:#3a2d4a,stroke:#74a,color:#fff
```

**关键设计（让三段式成立的前提）**：`PermissionSource` 接口
（`permission.go:43`）是**唯一接缝**——

```go
type PermissionSource interface {
    Allowed(ctx context.Context, req AccessRequest) (bool, error)
}
```

换实现**不改消费方**（`permission_plugin.go:52` 的 `beforeTool`）。
这是已存在的接缝（原注释即写明「换数据源只需换实现」），本需求只是**明确用它**。

### 3.3 方案 A vs 方案 B

| | 方案 A：自研（现状） | 方案 B：casbin |
|---|---|---|
| **实现** | `rbac.go` 200 行 | `casbin.Enforcer` + `model.conf` |
| **依赖** | 零 | `apache/casbin`（20.4k stars，已入 Apache 基金会） |
| **能力** | RBAC0 + RBAC1 | RBAC0-3 + ABAC + 资源级 + 多模型 |
| **心智** | 读 200 行 Go | 学 Casbin model 语法（`p`/`g`/`m` 段） |
| **资源级** | 待 v2（`AccessRequest.Resource` 已预留） | 内置支持（`p, sub, obj, act`） |
| **配置** | 环境变量（临时） | `model.conf` + `policy.csv`（标准） |
| **DB 后端** | 无 | adapter 生态（GORM/SQLite/Postgres） |

**收益/代价称量**：

- **引入 casbin 的真实收益**：① 资源级判定（`Resource` 字段）开箱即用，而自研需 v2 补；
  ② 配置格式标准化（`policy.csv` 可读性远好于环境变量）；③ 策略持久化到 DB 有现成 adapter。
- **引入 casbin 的真实代价**：① 新增依赖（用户想「慢慢减少依赖」）；
  ② 现有 13 例测试与 `Validate` 逻辑作废（需重写为 casbin 语义）；
  ③ **调试黑盒化**——自研 200 行可单步，casbin 的匹配失败要读它的内部日志。

### 3.4 推荐：**暂不引入 casbin，但把接缝留好**

**理由**：

1. **当前规模不需要**——`07-RBAC权限设计.md:65` 已论证：「个位数用户时 RBAC 是过度设计」。
   casbin 的资源级/ABAC 能力在**没有资源级需求时是空转**。
2. **已有实现质量足够**——200 行含防环、校验、继承，且测试完备（含反证）。
   替换它属于「用新依赖换掉能用的代码」，净收益为负。
3. **接缝已就位**——`PermissionSource` 就是替换点。等真需要资源级/DB 时，
   引入 casbin 是**新增一个文件**，不改任何消费方。

**什么条件下应该换成 casbin**（明确的触发条件，避免模糊）：

- 需要**资源级判定**（如「用户 A 只能写 `/data/a/` 下的文件」）——当前 `Resource` 字段已透传但不参与判定（`permission.go:105`）；
- 需要**策略持久化 + 热更新**（当前配置变更需重启）；
- 角色数 > 20 或权限点数 > 100（环境变量配置开始不可维护）。

### 3.5 数据库：同理，暂不引入

**现状**：`go.mod` 无任何数据库依赖（实测：`sqlite|gorm|database/sql|postgres|mysql` 零命中）。

**当前持久化**：会话历史由 trpc-agent-go 的 in-memory session service 承载
（`execute.go` 注释已记录：runner 持有 session service，默认 inmemory）。

**判断**：数据库要解决的是「进程重启后数据还在」。
当前原型**不要求跨重启持久化**——引入 DB 是为尚不存在的需求付成本。

**引入时机**（与 casbin 相同逻辑）：

- 需要**策略持久化**（RBAC 配置存 DB 而非环境变量）；
- 需要**审计日志落库**（FR-10.9 要求审计，但 v1 可先落文件）；
- 需要**会话历史跨重启**。

> **注意一处依赖耦合**：casbin 的 DB adapter 与「策略持久化」是同一个需求的两面。
> 若引入 casbin 是为了 DB，则两者应**同时**引入；若只为资源级判定，
> casbin 可用文件 adapter（`policy.csv`），仍无需 DB。

---

## 4. 与现有需求的关系（交叉引用核验）

| 现有条目 | 关系 | 处理 |
|---|---|---|
| FR-10.5 Owner 模型 | 命令系统需实现 `/owner_mention`、`/release_owner` | **实现该需求** |
| FR-10.6 跨面一致性 | 命令是「第五个面」，需与 HTTP/WS/IM/MCP 一致 | **扩展该需求** |
| FR-10.7 IM 用户降级 | RBAC 与它冲突（`07` 文档 §5 已论证改写） | **已被 `07` 改写** |
| FR-10.8 用户绑定 | RBAC 是其实现 | **已实现**（`07` Wave 2） |
| FR-10.9 审计 | 命令的拒绝/执行必须审计 | **实现该需求** |
| FR-10.10 命令豁免限流 | `/stop` 必须豁免 | **实现该需求**（当前限流未实现，需一并考虑） |

---

## 5. 验收标准

### 5.1 命令系统（FR-C）

| AC | 内容 | 验证方式 |
|---|---|---|
| AC-C1 | `/help` 列出全部已注册命令 | 单测：注册 N 个命令 → 输出含全部 N 个 |
| AC-C2 | 命令**不调用模型** | 单测：执行 `/help` → executor 调用计数 = 0 |
| AC-C3 | 命令**不写入会话历史** | 单测：执行命令后 session 消息数不变 |
| AC-C4 | 未注册的 `/xxx` 走正常消息路径 | 单测：`/usr/local/bin 是什么` → 进模型 |
| AC-C5 | 受限命令对无权限用户返回**明确拒绝** | 单测：viewer 执行 `/clear` → 拒绝文案含「权限」 |
| AC-C6 | `/help` 输出从注册表动态生成 | **反证**：新增命令后不更新帮助 → 测试应变红 |
| AC-C7 | 命令在私聊与群聊均可达 | 集成：两种 ChatType 各跑一遍 |

### 5.2 RBAC / 依赖（FR-D）

| AC | 内容 | 验证方式 |
|---|---|---|
| AC-D1 | `PermissionSource` 是唯一替换接缝 | 静态断言：消费方只依赖接口，不依赖具体实现 |
| AC-D2 | 换实现不改消费方 | 单测：用 fake `PermissionSource` 装配 → 插件行为不变 |
| AC-D3 | 命令权限点走 RBAC（非独立集合） | 单测：给角色配 `cmd:clear` → 该角色可用 `/clear` |
| AC-D4 | 角色继承对命令权限生效 | 单测：admin 继承 operator 的命令权限 |
| AC-D5 | 现有 RBAC 测试全绿（引入命令后不回归） | `go test ./internal/authz/ ./cmd/taiji/` |

### 5.3 文档一致性

| AC | 内容 | 验证方式 |
|---|---|---|
| AC-DOC1 | 本文档所有 `file:line` 锚点属实 | 逐条 grep 核验（见 §7） |
| AC-DOC2 | 定位表述与 `01-需求文档.md` 一致 | ✅ **已同步**（2026-09-24）：`01` 改为两层表述 + 新增 G7/N6 |

---

## 6. 风险

| 风险 | 影响 | 缓解 |
|---|---|---|
| **命令与消息的边界模糊** | 用户发 `/usr/bin 在哪` 被误判为命令 | FR-C1 明确：未注册命令名走正常路径 |
| **两套权限真相** | 命令走独立集合 + RBAC 走权限点 → 不一致 | FR-C3 明确：命令权限点是 RBAC 权限点的一种 |
| **casbin 过早引入** | 依赖膨胀 + 现有测试作废 | §3.4 给出明确的触发条件，不模糊 |
| **定位表述不一致** | 两份文档自我描述冲突 | ✅ **已解决**：`01` 已同步为两层表述（产品定位 + 实现形态） |
| **限流未实现** | FR-10.10 的「命令豁免限流」无对象可豁免 | 限流实现时需一并做，或明确记录该缺口 |

---

## 7. 锚点核验记录

本文档引用的所有 `file:line` 均经 grep 实测核验（2026-09-24）：

| 锚点 | 内容 | 核验 |
|---|---|---|
| `internal/authz/permission.go:30` | `type AccessRequest struct` | ✓ |
| `internal/authz/permission.go:43` | `type PermissionSource interface` | ✓ |
| `internal/authz/permission.go:69` | `type StaticPermissions struct` | ✓ |
| `internal/authz/permission.go:105` | `StaticPermissions.Allowed` | ✓ |
| `internal/authz/permission.go:127` | `matchToolPattern` | ✓ |
| `internal/authz/rbac.go:26` | `type RBACConfig struct` | ✓ |
| `internal/authz/rbac.go:42` | `type RBACPermissions struct` | ✓ |
| `internal/authz/rbac.go:78` | `collectPermissions`（继承展开） | ✓ |
| `internal/authz/rbac.go:129` | `RBACConfig.Validate` | ✓ |
| `internal/authz/rbac.go:170` | `RBACPermissions.Allowed` | ✓ |
| `internal/authz/permission_plugin.go:52` | `beforeTool`（判定入口） | ✓ |
| `internal/authz/permission_plugin.go:128` | `denyMessage`（拒绝文案） | ✓ |
| `internal/authz/context.go:22` | `KindInteractive` | ✓ |
| `internal/authz/context.go:28` | `KindChannel` | ✓ |
| `cmd/taiji/main.go:67-73` | CLI 子命令 switch | ✓ |
| `docs/01-需求文档.md:590` | 命令豁免限流 | ✓ |

> **锚点漂移修正记录（2026-09-24）**：`01-需求文档.md` 因定位同步在头部插入了 38 行，
> 其「命令豁免限流」条目由 `:552` 漂移至 `:590`。本文档 3 处引用已同步修正
> （§2.1、§2.2 表格、本表）。**这是文档改动的连带影响**——改被引文档必须复核引用方锚点。
| `happyclaw/src/commands.ts:2` | 命令在管道前拦截 | ✓ |
| `happyclaw/src/im-command-utils.ts:272` | `OWNER_REQUIRED_IM_COMMANDS` | ✓ |

---

## 8. 待用户确认的问题

> **原有两个问题，其一已解决**：
> ~~定位表述~~ → ✅ **用户已拍板同步**（2026-09-24），`01-需求文档.md` 已改为两层表述，见 §1。

**剩余待确认：casbin 时机**

本文档推荐**暂不引入**（§3.4 给出三条理由 + 三个触发条件）。
若用户希望现在就引入（例如为了「先用开源生态跑通」），
实现成本约「新增 1 个文件 + 重写 13 例测试」——请明确指示。

---

## 附录：本需求与 `07-RBAC权限设计.md` 的分工

两份文档**不重复**，边界如下：

| 文档 | 回答的问题 | 层次 |
|---|---|---|
| `07-RBAC权限设计.md` | RBAC **怎么实现**（接口、决策、代价、缺口） | 设计 |
| 本文档（`08`） | RBAC **要不要引入 casbin/DB**、**命令系统要什么** | 需求 |

`07` 是既有设计（已落代码），本文档是其**上游需求补充**——
新增了命令系统这条 `07` 未覆盖的需求，并对依赖策略给出决策框架。
