# 15 - Windows 本地开发部署

> 生成于 2026-10-02，对应提交 `408dc26`。
>
> **本文档的性质**：操作指南——照着做能从零跑到服务起来。
> 文中每条命令与输出**均在本机实跑**（记录见每节末尾的「实测」标注）。
>
> 配套文档：`13-环境变量清单.md`（配置项完整参考）、
> `14-全局设计文档.md`（设计原理与生效范围）。

---

## 1. 前置条件

| 项 | 要求 | 核验命令 |
|---|---|---|
| 操作系统 | Windows 10/11（本文实测于 Windows_NT 10.0.26200） | `[System.Environment]::OSVersion` |
| Go | **1.27.1**（`go.mod` 的 `go` 指令） | `go version` |
| 网络 | 能出网（长连接模式只需出网，**不需要公网入口**） | — |
| 飞书应用 | 至少一个自建应用（`app_id` / `app_secret`） | — |
| LLM 端点 | OpenAI 兼容的 `base_url` / `api_key` / 模型名 | — |

**实测**：本机 `go version` → `go version go1.27.1 windows/amd64`。

> **Go 版本若低于 1.27.1**，`go.mod` 的 `go 1.27.1` 指令会让构建直接失败，
> 报 `go.mod requires go >= 1.27.1`。先升级 Go，不要改 `go.mod` 里的版本号
> ——那会让依赖解析行为与设计不符。

---

## 2. 构建

```powershell
cd E:\aiops\TaiJi
go build -o taiji.exe ./cmd/taiji
```

**实测**：冷构建（无缓存）约 **32 秒**，产物 `taiji.exe` 约 **55 MB**。
`go.sum` 已锁定依赖，首次构建会自动下载（约 15 KB 的 sum 文件对应完整依赖树）。

**交叉编译**（若要在 Linux 部署，在 Windows 上构建 Linux 产物）：

```powershell
$env:GOOS="linux"; $env:GOARCH="amd64"
go build -o taiji-linux ./cmd/taiji
Remove-Item Env:GOOS, Env:GOARCH   # 用完清理，否则影响后续构建
```

### 构建产物说明

| 文件 | 用途 |
|---|---|
| `taiji.exe` | 主程序 |
| `mockmcp.exe` | **测试用** MCP server（本地联调工具调用时用，生产不需要） |

> **每次改代码后必须重新构建**。本会话踩过一次：改了多 agent 逻辑却跑了旧的
> `taiji.exe`，行为与代码不符，排查方向被带偏。判断二进制是否最新：
> `(Get-Item taiji.exe).LastWriteTime` 应晚于最后一次提交时间。

---

## 3. 最简可跑配置（单 agent）

### 3.1 设置环境变量（PowerShell）

```powershell
# ── 模型（必填，缺任一则拒绝启动）──
$env:TAIJI_MODEL_NAME     = "deepseek-chat"
$env:TAIJI_MODEL_API_KEY  = "sk-xxxxxxxx"
$env:TAIJI_MODEL_BASE_URL = "https://api.example.com/v1"   # ⚠ 必须自带 /v1

# ── 飞书应用凭据（必填）──
$env:FEISHU_APP_ID     = "cli_xxxxxxxxxxxx"
$env:FEISHU_APP_SECRET = "xxxxxxxxxxxxxxxxxxxxxxxx"

# ── 群聊 @ 判定基准（必填，否则群消息全被拒）──
$env:TAIJI_FEISHU_BOT_OPEN_ID = "ou_xxxxxxxxxxxx"
```

> ### ⚠ `TAIJI_MODEL_BASE_URL` 必须自带 `/v1`
>
> SDK 的拼接方式是 `BaseURL + "/chat/completions"`。若只写
> `https://api.example.com`，实际请求会打到 `/chat/completions`（缺 `/v1`），
> 得到 404 或 "invalid param"。
>
> **这条来自实际踩坑经验，不是从源码读出的结论**（拼接逻辑在依赖内部）。

### 3.2 启动

```powershell
.\taiji.exe serve --feishu-mode=longconn
```

**成功时的输出**（实测，对应 §3.1 的最简配置）：

```
生效的控制值（来自受信启动环境）:
2026/10/02 16:22:26 [Info] [client ready]
[pipeline] MCP server 已装配：[]
[pipeline] 命令系统已装配：[help whoami status clear stop]
[pipeline] 警告：未配用户级权限源，命令将全部被拒（fail-closed）。…例外：/whoami 无需权限，可先用它查出自己的主体 ID（配 RBAC 要用）
2026/10/02 16:22:26 [Info] [event-dispatch is ready]
taiji serve: 长连接已启动（1 条，无需公网入口）
```

看到 **`长连接已启动`** 即成功。Ctrl+C 优雅退出（会先停长连接、再停分发器）。

> **那条「命令将全部被拒」的警告是预期的**——§3.1 没配权限。它**只影响斜杠
> 命令**（`/help` `/status` `/clear` `/stop`），工具调用与普通对话不受影响。
> `/whoami` 也可用（免权限）。配好 §5.2 的 RBAC 后该警告消失。

> **若凭据是假的**，还会看到（实测）：
> ```
> [Warn] ... get conn url failed, err: 1000040346: app_id is invalid
> [Error] ... connect failed, err: 1000040346: app_id is invalid
> ```
> 这**不影响**「长连接已启动」这行——`Start` 是异步的，连接失败在后台回调里报。
> 故**判断启动是否成功不能只看那行**，还要确认没有连接错误。

### 3.3 环境变量的作用域（PowerShell 的坑）

`$env:X = "v"` 只在**当前窗口**有效。换窗口、换 IDE、开新终端都会丢。

**持久化**（写入用户级注册表，新开的窗口生效）：

```powershell
[Environment]::SetEnvironmentVariable("TAIJI_MODEL_NAME", "deepseek-chat", "User")
```

> **实测过的坑**：用 `setx` 写入后，**当前窗口不会立即生效**（注册表改了，
> 但进程环境块已固化）。必须**新开一个终端**才能读到。排查「配了却没生效」
> 时先确认这一点，别急着怀疑代码。

---

## 4. 多 agent 配置（每 agent 一个飞书应用）

```
$env:TAIJI_AGENTS = "name=root,app_id=cli_aaa,app_secret=s1,instruction=你是调度者;" +
                    "name=bill,app_id=cli_bbb,app_secret=s2,parent=root,instruction=你是账单助手"
```

> **注意**：PowerShell 里长字符串用 `+` 拼接，**不要**用 bash 的反斜杠续行。

**启动后会看到**（实测）：

```
[pipeline] 多 agent 模式：[root bill]（每个 agent 独立飞书应用与 session）
[pipeline] 门禁：已按 agent 取 bot open_id（2 个）
taiji serve: 多 agent 长连接已装配 2 条
taiji serve: 长连接已启动（2 条，无需公网入口）
```

**关键差异**：多 agent 时

- **不必设** `FEISHU_APP_ID` / `FEISHU_APP_SECRET`（除非全局 sender 需要，
  见 §7.2）
- **不必设** `TAIJI_FEISHU_BOT_OPEN_ID`——启动时会按各 agent 凭据自动调
  `/open-apis/bot/v3/info` 取 `open_id`（`门禁：已按 agent 取 bot open_id（N 个）`）
- 取不到即**中止启动**并点名是哪个 agent

**每 agent 可选字段**（写在同一行，逗号分隔）：

| 字段 | 作用 |
|---|---|
| `parent=` | 父 agent 名（父可 `transfer_to_agent` 到子） |
| `instruction=` | 该 agent 的系统提示 |
| `skills=` | skill 仓库根目录 |
| `allow_tools=` | MCP 工具白名单，**用 `\|` 分隔**（逗号已是字段分隔符） |

---

## 5. 权限配置

### 5.1 先查出自己的主体 ID

在飞书里给 bot 发 **`/whoami`**（**无需任何权限配置**即可用）：

```
你的身份：
  主体 ID：default:feishu:on_2fb858167bfd7627928743231966355
  配置位置：TAIJI_RBAC 的 user:<上面这串>=<角色名>
  Agent：bill
  身份键来源：union_id（跨应用稳定）
    同一个你在所有 bot 下都是上面这个 ID——配置一次即可。
```

**复制「主体 ID」那行**用于下一步。它与实际判定用的值**同源**，不会出现
「看起来对但匹配不上」。

### 5.2 配置 RBAC

```powershell
$env:TAIJI_RBAC = "role:operator=cmd:help,cmd:status,infraverse_*;" +
                  "user:default:feishu:on_2fb858167...=operator"
```

格式（`;` 分隔条目，`=` 左键右值，值内 `,` 分隔）：

| 条目 | 含义 |
|---|---|
| `role:<名>=<权限点,...>` | 角色 → 权限点 |
| `parent:<子角色>=<父角色>` | 角色继承 |
| `user:<主体ID>=<角色,...>` | 用户 → 角色 |

**权限点两种形态**：

- **裸模式**（`cmd:help` / `infraverse_*` / `*`）→ **全局**生效
- **`agent:<名>:<模式>`** → 仅该 agent 生效

**命令权限点必须显式列出**（形如 `cmd:help`）——它们不匹配工具名规则。
**例外**：`/whoami` 无需权限（它的用途正是查出该配什么权限）。

### 5.3 不想按用户管控时

若确认是单用户/只读场景：

```powershell
$env:TAIJI_ALLOW_ALL_USERS = "1"
```

> ### ⚠ 这个开关的**实际语义**（实测澄清，与直觉不同）
>
> 它让 **serve 允许启动**，但 `permissions` 仍为 nil。后果**不对称**：
>
> | | 行为 | 依据 |
> |---|---|---|
> | **工具调用** | **放行**（权限插件根本不装配） | `internal/chat/execute.go:365` 的 `if opts.Permissions != nil` |
> | **命令**（`/help` 等） | **全部被拒** | `internal/server/pipeline.go:715` 显式 `nil → false` |
>
> 所以启动日志里会**同时**出现这两条（实测）：
>
> ```
> [pipeline] 用户级权限：已按 TAIJI_ALLOW_ALL_USERS=1 显式放开——任何能触发 bot 的用户都可使用已放行的工具。
> [pipeline] 警告：未配用户级权限源，命令将全部被拒（fail-closed）。…例外：/whoami 无需权限…
> ```
>
> **两条说的不是同一件事**（一条讲工具、一条讲命令），不矛盾——
> 但并排出现容易误读成「既放开又全拒」。**以「命令将全部被拒」为准**：
> 工具可用，斜杠命令（除 `/whoami`）不可用。

---

## 6. MCP 工具接入

### 6.1 本地 stdio server

```powershell
$env:TAIJI_MCP_SERVERS = "mock=.\mockmcp.exe"
$env:TAIJI_ALLOW_TOOLS  = "mock_echo"
```

### 6.2 远程 SSE server（需认证头）

```powershell
$env:TAIJI_MCP_SERVERS      = "infra=http://10.0.0.1:31177/sse"
$env:TAIJI_MCP_HEADERS_INFRA = "Authorization:Bearer xxxxxxxx"
```

头部变量名是 **`TAIJI_MCP_HEADERS_<SERVER名大写>`**——与 `TAIJI_MCP_SERVERS`
里的 server 名对应。

### 6.3 ⚠ 白名单必须写「模型可见名」

挂载 server 后，工具名会被上游加前缀，变成 **`{server名}_{原始工具名}`**：

```
TAIJI_MCP_SERVERS="mock=.\mockmcp.exe"   →  工具名是 mock_echo（不是 echo）
```

**写错会在装配期报错**，并打印实际可用的工具名（`工具策略：默认拒绝，放行 [...]`）。

**实测的成功输出**：

```
[pipeline] MCP server 已装配：[mock]
[pipeline] 模型可见的工具名：[mock_echo]
[pipeline] 工具策略：默认拒绝，放行 [mock_echo]
```

**默认拒绝**：`TAIJI_ALLOW_TOOLS` 为空 ⇒ 所有工具不可执行。

---

## 7. 常见启动失败与处置

以下错误信息均为**实测原文**。

### 7.1 缺飞书凭据

```
taiji serve: feishu: outbound requires FEISHU_APP_ID and FEISHU_APP_SECRET (set them in the startup environment)
```

**exit=1**。设 `FEISHU_APP_ID` / `FEISHU_APP_SECRET`（多 agent 时改设 `TAIJI_AGENTS`）。

### 7.2 多 agent 仍要求全局凭据

多 agent 配置完整，却报同样的错——这是**已知的装配顺序**：
`buildPipeline` 会构造一个全局 sender（多 agent 下**不会被用到**，但
`server.New` 要求非 nil）。

**处置**：多 agent 时**顺手也设上** `FEISHU_APP_ID` / `FEISHU_APP_SECRET`
（填任意一个 agent 的凭据即可，反正不用）。这是配置面的冗余要求，
不是功能缺陷。

### 7.3 缺模型配置

```
taiji serve: 装配执行器: bootstrap: model name is required (set TAIJI_MODEL_NAME or config field "name")
```

**exit=1**。设 `TAIJI_MODEL_NAME`（另两个模型变量也建议一并设上）。

### 7.4 群聊里 @ 了但 bot 不理

按顺序排查：

1. **看日志有无 `gate rejected`**
   - `reason=not_mentioned` → @ 判定失败。检查 `TAIJI_FEISHU_BOT_OPEN_ID`
     是否为**该 bot 的** open_id（多 agent 下每个 bot 不同，多 agent 模式会自动取）
   - `reason=bot_open_id_missing` → 没设 `TAIJI_FEISHU_BOT_OPEN_ID`（单 agent）
2. **无任何日志** → 消息可能没投递到该 bot（检查应用是否在群里、长连接是否建立）
3. **有 `[perm] denied`** → 权限问题，不是门禁问题。

### 7.5 MCP server 起不来

```
taiji serve: 装配 MCP: mcp server "xxx" (sse url=...): failed to connect to MCP server: ...
```

MCP 装配失败**即中止启动**（不静默降级）。先用 `curl`/PowerShell 确认端点可达，
再检查 `TAIJI_MCP_HEADERS_<SERVER>` 的认证头格式。

> **实测的构造坑**：用 `TAIJI_MCP_SERVERS="srv=echo hi"` 之类的**非 MCP 程序**
> 当 server，会看到一堆 `Error reading message: invalid character ...`。
> 那是 server 不实现 MCP 协议所致——本地联调请用 `mockmcp.exe`。

---

## 8. 工作区配置文件（可选）

`--config` 指定的文件（示例：`configs\taiji.example.conf`）可提供**普通键**：

```powershell
.\taiji.exe serve --config configs\taiji.example.conf
```

### 8.1 它真正有用的地方：`WORKSPACE_NAME`

**这是工作区文件目前唯一有生产用途的键**——它是 `workspaceID()` 的第二优先级
回退（`cmd/taiji/main.go:799`）：

```
TAIJI_WORKSPACE_ID 环境变量  >  工作区文件的 WORKSPACE_NAME  >  "default"
```

`workspaceID` 决定：**串行化域、会话路由前缀、RBAC 主体 ID 的 workspace 段**。

**实测**（`WORKSPACE_NAME=myws`）：

```
[pipeline] 主体 ID 前缀：myws:feishu: —— 配置的 key 应写成 <该前缀><用户open_id>，如 myws:feishu:ou_xxx
```

> ### ⚠ 改它会连带改主体 ID
>
> 主体 ID 从 `default:feishu:on_xxx` 变成 `myws:feishu:on_xxx`，
> **已配好的 `TAIJI_RBAC` 用户条目会全部失配**（表现为「突然没权限了」）。
> 改这个值前先想清楚——建议同步改 RBAC，或用 `/whoami` 核对新 ID。

### 8.2 另一个可覆盖项（有限）

`TAIJI_MODEL_BASE_URL` 可通过工作区文件覆盖环境变量
（`cmd/taiji/main.go:133,439` 从 loaded 读）。

> `TAIJI_MODEL_NAME` 虽然代码里也读 loaded（`main.go:130`），但它**同时在
> `ReservedKeys` 里**，写在工作区文件会被跳过——所以实际能覆盖的只有
> `BASE_URL`。这是保护基线的一部分，不是遗漏。

### 8.3 保留键写在这里会被跳过并告警

理由：工作区是 **agent 可写区域**，允许它覆盖控制值等于开提权路径。

| 类别 | 键 |
|---|---|
| 控制值 | `TAIJI_INSTRUCTION`、`TAIJI_SKILLS_ROOT`、`TAIJI_SKILL_TOOL_PROFILE`、`TAIJI_AGENTS` |
| 凭据 | `FEISHU_APP_ID`、`FEISHU_APP_SECRET`、`FEISHU_VERIFICATION_TOKEN`、`FEISHU_ENCRYPT_KEY` |
| 前缀族 | `TAIJI_MCP_HEADERS_*` |
| 历史遗留（保留作保护基线） | `MODEL_ROUTE`、`EXECUTION_MODE`、`SANDBOX_POLICY`、`CREDENTIAL_REF`、`MCP_SERVERS`、`TOOL_POLICY` |

> **凭据一律走环境变量，不落文件**——这条是硬约束，不是建议。

---

## 9. 完整示例：本地全功能启动

```powershell
# ── 1. 模型 ──
$env:TAIJI_MODEL_NAME     = "deepseek-chat"
$env:TAIJI_MODEL_API_KEY  = "sk-xxxxxxxx"
$env:TAIJI_MODEL_BASE_URL = "https://api.example.com/v1"

# ── 2. 飞书（单 agent）──
$env:FEISHU_APP_ID            = "cli_xxxxxxxxxxxx"
$env:FEISHU_APP_SECRET        = "xxxxxxxxxxxxxxxx"
$env:TAIJI_FEISHU_BOT_OPEN_ID = "ou_xxxxxxxxxxxx"

# ── 3. 工具 ──
$env:TAIJI_MCP_SERVERS = "mock=.\mockmcp.exe"
$env:TAIJI_ALLOW_TOOLS = "mock_echo"

# ── 4. 权限（先发 /whoami 拿到主体 ID，再填这里）──
$env:TAIJI_RBAC = "role:operator=cmd:help,cmd:status,mock_echo;user:<你的主体ID>=operator"

# ── 5. 启动 ──
.\taiji.exe serve --feishu-mode=longconn
```

**跳过权限配置的快速验证**（只想确认链路通）：

```powershell
$env:TAIJI_ALLOW_ALL_USERS = "1"   # 注意 §5.3 的语义差异
```

---

## 10. 本地联调（不接真飞书）

用 `chat` 子命令直接在终端对话——**不经过飞书，也不需要飞书凭据**：

```powershell
$env:TAIJI_MODEL_NAME     = "deepseek-chat"
$env:TAIJI_MODEL_API_KEY  = "sk-xxxxxxxx"
$env:TAIJI_MODEL_BASE_URL = "https://api.example.com/v1"

go run ./cmd/taiji chat --mcp "mock=.\mockmcp.exe" --allow-tool mock_echo --debug
```

| flag | 作用 |
|---|---|
| `--mcp` | 挂 MCP server（可重复） |
| `--allow-tool` | 放行工具名（可重复） |
| `--instruction` | 覆盖系统提示 |
| `--debug` | 打印每轮调试信息（含多轮历史观察点） |

> **CLI 不消费权限配置**——若设了 `TAIJI_RBAC`，`chat` 会提示
> 「该配置仅 serve 生效」（权限按 IM 主体判定，CLI 无 IM 身份）。

---

## 11. 附：本文档的核验方式

```powershell
# 每个子命令的 flag 以 --help 为准（本文档若与 --help 冲突，以 --help 为准）
.\taiji.exe --help
.\taiji.exe serve --help
.\taiji.exe chat --help

# 环境变量完整清单见 docs/13-环境变量清单.md
```

**本文档的局限**：

- 命令与输出实测于 **Windows_NT 10.0.26200 + Go 1.27.1**，其他版本可能有差异
- **未实测**：真实飞书租户的完整链路（需真实应用凭据）、Linux 部署、
  大规模并发下的长连接稳定性
- 编译期命令（`go build`）已验证；**运行期的飞书连通性未在本机验证**
  （本文档 §7 的错误信息均来自配置校验阶段，非网络阶段）
