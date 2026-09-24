# Taiji · MCP 配置手册（Windows）

> 项目：**taiji**（Go 原生 Agent Harness 原型）
> 版本：**v0.1**
> 日期：2026-09-24
> 配套：`01-需求文档.md`（v1.4）、`02-设计文档.md`（v1.4）、`03-原型设计文档.md`（v0.1）
> 定位：**MCP 工具的配置与验证操作手册**——覆盖 stdio / 远程两种形态、静态 token 认证、以及三条配置路径的一致性
> 适用代码：`93ec694`（MCP 远程+认证）、`650aabc`（CLI 配置面）、`9e23574`（serve 可观测性）

---

## 1. 快速开始

### 1.1 三组环境变量

| 变量 | 作用 | 必填 |
|---|---|---|
| `TAIJI_MCP_SERVERS` | 挂载哪些 MCP server | 用 MCP 时必填 |
| `TAIJI_ALLOW_TOOLS` | 放行哪些工具（逗号分隔） | 用 MCP 时必填 |
| `TAIJI_MCP_HEADERS_<server名>` | 远程 server 的认证头 | 仅远程+需认证时 |

**最关键的一点**：工具策略是**默认拒绝 + 白名单放行**（§4.1 详述）。不设 `TAIJI_ALLOW_TOOLS`，配了 server 也一个工具都跑不了。

### 1.2 最小可用配置（stdio）

在 **CMD 窗口**执行：

```cmd
setx TAIJI_MCP_SERVERS "mockmcp=go run ./testdata/mockmcp"
setx TAIJI_ALLOW_TOOLS "mockmcp_echo"
```

开**新** CMD 窗口（`setx` 对当前窗口不生效），启动：

```cmd
cd E:\aiops\TaiJi
.\taiji.exe chat
```

看到这两行即成功：

```
[mcp] mockmcp 已就绪
工具策略：默认拒绝，放行 [mockmcp_echo]
```

### 1.3 编译产物

代码改动后需重新构建（`taiji.exe` 不会自动更新）：

```cmd
go build -o taiji.exe ./cmd\taiji
```

---

## 2. 配置语法

### 2.1 `TAIJI_MCP_SERVERS`

分号分隔多个 server，每个条目的形态由 `=` 右侧决定：

| 形态 | 语法 | transport |
|---|---|---|
| 本地子进程 | `名=命令 参数...` | `stdio` |
| 远程（推荐） | `名=https://host/mcp` | `streamable` |
| 远程（SSE） | `名=https://host/sse` | `sse` |

判定规则（`cmd/taiji/main.go` 的 `mcpTransportForURL`）：

- `http://` 或 `https://` 前缀 → 远程
- URL 以 `/sse` 结尾（允许尾斜杠）→ `sse`
- 其余 → `streamable`（MCP 2025 规范推荐形态）

**多 server 例子**：

```cmd
setx TAIJI_MCP_SERVERS "mockmcp=go run ./testdata/mockmcp;github=https://api.githubcopilot.com/mcp/"
```

### 2.2 `TAIJI_ALLOW_TOOLS`

**逗号**分隔（与 `TAIJI_FEISHU_OWNERS` 同约定）：

```cmd
setx TAIJI_ALLOW_TOOLS "mockmcp_echo,github_search_code,github_get_file_contents"
```

**工具名是「模型可见名」，带 server 前缀**。MCP 的裸工具名经 llmagent 的 `NamedToolSet` 包装后变成 `{server名}_{裸工具名}`（依据 `trpc-agent-go@v1.11.2/internal/tool/toolset.go:251`）。

例：server 名 `mockmcp` + 裸工具 `echo` → 白名单里要写 `mockmcp_echo`。

**⚠ 大小写敏感**。`Infraverse_dce_ip` 与 `infraverse_dce_ip` 是两个不同的名字。若 server 名写成大写开头（如 `Infraverse`），白名单必须逐字匹配——包括远端工具名部分可能自带的 `infraverse_` 前缀，最终形如 `Infraverse_infraverse_dce_ip`。

**写错名的后果**：`BuildToolPolicy` 会校验白名单条目是否存在于已注册工具中，未注册的名字会**报错并列出已注册名**（不静默忽略）——这是 issue #4 的有意设计，避免「配了却没生效」难以排查。实际输出：

```
taiji serve: 装配执行器: authz: allow list names 1 tool(s) that are not
             registered: infraverse_dce_ip (registered: Infraverse_infraverse_dce_ip)
```

`registered:` 后面就是该填进白名单的名字。启动**成功**时也会打印可用工具名：

```
[pipeline] MCP server 已装配：[mockmcp]
[pipeline] 模型可见的工具名：[mockmcp_echo]
[pipeline] 工具策略：默认拒绝，放行 [mockmcp_echo]
```

### 2.3 `TAIJI_MCP_HEADERS_<server名>`

格式：`头名:值`，多个头用**分号**分隔。

```cmd
setx TAIJI_MCP_HEADERS_github "Authorization:Bearer ghp_你的token"
```

多个头：

```cmd
setx TAIJI_MCP_HEADERS_github "Authorization:Bearer ghp_xxx;X-Api-Key:abc123"
```

**变量名的后缀必须与 `TAIJI_MCP_SERVERS` 里 `=` 左边的名字完全一致**。

**解析细节**（`mcpHeadersFor`）：

- 只切**第一个**冒号，值内的冒号保留（`X-Ref:https://host:8443/path` 的值完整保留）
- 无冒号的条目**跳过**而非报错（避免一处笔误导致启动失败）
- 每个 server 的头互相隔离，不串味

**仅对远程 server 生效**——stdio 子进程没有 HTTP 头的概念，配了也不会用。

---

## 3. 配置路径与优先级

三条路径**共用同一套环境变量**，但各有额外的 flag 通道：

| 路径 | 启动命令 | MCP 配置来源 | 白名单来源 |
|---|---|---|---|
| CLI 对话 | `taiji chat` | `TAIJI_MCP_SERVERS` + `--mcp` flag | `TAIJI_ALLOW_TOOLS` + `--allow-tool` flag |
| Serve（webhook） | `taiji serve` | `TAIJI_MCP_SERVERS` | `TAIJI_ALLOW_TOOLS` |
| Serve（长连接） | `taiji serve --feishu-mode=longconn` | 同上 | 同上 |

**合并语义**：环境变量在前，flag 追加在后（`envMCPSpecs() + mcpSpecs`）。

**Serve 的两种模式共用同一管道**——`buildPipeline` 在 mode 分支**之前**调用（`cmd/taiji/main.go:304` vs `:325`），故 webhook 与 longconn 的 MCP 配置完全一致。

### 3.1 配置来源对照

| 变量 | 读取方式 | 工作区文件可否覆盖 |
|---|---|---|
| `TAIJI_MCP_SERVERS` | `os.Getenv` | 否 |
| `TAIJI_ALLOW_TOOLS` | `os.Getenv` | 否 |
| `TAIJI_MCP_HEADERS_*` | `os.Getenv` | **否（前缀保护）** |
| `TAIJI_MODEL_*` | `os.Getenv` | 否 |
| `WORKSPACE_NAME` | 经 `config.Load` | 是（普通键） |

**认证头必须走环境变量，不要写进 `--config` 工作区文件**。理由见 §4.3。

---

## 4. 设计决策与安全边界

### 4.1 工具策略默认拒绝

`internal/authz/toolpolicy.go:76` 装配 `approval.WithDefaultToolPolicy(approval.ToolPolicyDenied)`——空白名单等于全部拒绝。这是**安全基线**：不显式放行的工具一律不能跑。

对应测试：`TestBuildToolPolicy_DefaultIsDenied`、`TestEnvAllowTools_EmptyResultsInDeniedPolicy`。

**代价**：配置 MCP 时必须同时配白名单，多一步操作。这是刻意的取舍——「配了 MCP 就能跑任意工具」的风险高于「多配一个变量」的成本。

### 4.2 MCP 装配是启动期阻塞且 fail-fast

若 MCP server 起不来，**整个进程启动失败**：

```
taiji serve: 装配 MCP: mcp server "mockmcp" (stdio command=...): failed to connect to MCP server
```

依据 issue #3 AC-3 的 fail-fast 要求。

**代价**：配一个挂掉的 MCP server 会让飞书 bot 完全起不来，而不是「工具不可用但 bot 正常」。**收益**：不会让你误以为 bot 在正常工作。

### 4.3 认证头的前缀保护

`TAIJI_MCP_HEADERS_<server>` 含变量部分（server 名），无法静态枚举，故 `internal/config/guard.go` 新增 `ReservedPrefixes` 做**前缀**保护：

```go
var ReservedPrefixes = []string{
    "TAIJI_MCP_HEADERS_",
}
```

工作区文件提供的同前缀键会被**跳过并告警**（`MergeWorkspaceEnv`）。

**威胁模型**：工作区是 agent 可写区域。若 MCP token 能从那里读，攻击者改写一个文件就能把 agent 的 MCP 调用导到自己的 server——凭据劫持，与飞书凭据（`CredentialKeys`）同一模型。

对应测试：`TestMCPHeadersPrefix_ProtectedFromWorkspace`、`TestMCPHeadersPrefix_WorkspaceOnlyKeyIsSkipped`。

---

## 5. 验证方法

### 5.1 三步验证

**第 1 步：确认挂载**

启动后看输出。**CLI 路径**：

```
[mcp] mockmcp 已就绪
工具策略：默认拒绝，放行 [mockmcp_echo]
```

**Serve 路径（含长连接）**：

```
[pipeline] MCP server 已装配：[mockmcp]
[pipeline] 模型可见的工具名：[mockmcp_echo]
[pipeline] 工具策略：默认拒绝，放行 [mockmcp_echo]
```

若最后一行显示 `白名单为空——N 个已注册工具均不可执行`，说明 `TAIJI_ALLOW_TOOLS` 没生效。

**注意**：这三行只在**装配成功**后打印。若白名单名字写错，进程会在装配时失败退出（fail-fast），此时输出的是诊断提示而非这三行——详见 §2.2 的错误示例。

**第 2 步：确认工具可调用**

在对话里输入：

```
请调用 mockmcp_echo 工具，message 参数填"你好"
```

预期回答含 `Echo: 你好`——这是 mockmcp 的真实返回值（`prefix + message`，默认 `prefix="Echo: "`）。模型无法凭空编造该前缀，故它的出现即证明工具**真的被执行**。

**第 3 步：远程 + 认证**

```cmd
setx TAIJI_MCP_SERVERS "github=https://api.githubcopilot.com/mcp/"
setx TAIJI_MCP_HEADERS_github "Authorization:Bearer ghp_你的token"
setx TAIJI_ALLOW_TOOLS "github_search_code"
```

启动时若看到：

```
[pipeline] 提示：MCP server "github" 是远程（streamable）但未配置认证头。
```

说明认证头**没读进去**——检查变量名拼写（后缀须与 server 名一致）、或 `setx` 后是否开了新窗口。

### 5.2 自动化测试

MCP 相关测试共 **29 条**（`cmd/taiji/mcpauth_test.go` 22 条 + `cmd/taiji/allowtools_test.go` 7 条），分四层：

| 层 | 代表测试 | 验证什么 |
|---|---|---|
| 解析 | `TestParseMCPSpecs_RemoteHTTPS`、`TestParseMCPSpecs_SSEWithTrailingSlash` | spec → 配置（transport/URL/Headers） |
| 认证头解析 | `TestMCPHeadersFor_ColonInValuePreserved`、`TestMCPHeadersFor_PerServerIsolation` | 格式解析、值内冒号保留、多 server 隔离 |
| 安全 | `TestMCPHeadersPrefix_ProtectedFromWorkspace` | 工作区不可覆盖认证头 |
| **端到端** | `TestMCPHeaders_ReachHTTPRequest` | 对假 MCP server 实测 `Authorization` 真的到达 HTTP 请求 |
| 接线 | `TestRunChat_ReadsMCPFromEnv`、`TestBuildPipeline_PassesAllowToolsToExecutor` | CLI/serve 两条路径都读了环境变量 |

**端到端测试的必要性**：`Headers` 从 `MCPServerConfig` 到 `http.Request` 之间隔着 SDK 的 `buildHTTPOptions`（`trpc-agent-go@v1.11.2/tool/mcp/toolset.go:293-301`），只有实测能证明它到达了线缆。该测试做了**反向验证**——删除 `internal/bootstrap/mcp.go:95` 的 `Headers: c.Headers` 透传后，测试 FAIL（`Authorization = ""`）。

运行：

```cmd
go test ./cmd/taiji/ -run "MCP|ParseMCPSpecs|EnvAllowTools" -v
```

---

## 6. Windows 实操要点

### 6.1 `set` 与 `setx` 的区别

| 命令 | 生效范围 | 持久性 |
|---|---|---|
| `set X=1` | 仅当前窗口 | 否 |
| `setx X "1"` | 新开的窗口 | 是（写 `HKCU\Environment`） |

调试时用 `set`（改完立即生效），定型后用 `setx`。

### 6.2 三个实测确认的坑

**坑 1：`setx` 的值必须加引号**

```cmd
setx TAIJI_MCP_SERVERS mockmcp=go run ./testdata/mockmcp     ← 错误：报「无效语法」
setx TAIJI_MCP_SERVERS "mockmcp=go run ./testdata/mockmcp"   ← 正确
```

值里的空格会被拆成多个参数。实测：不加引号时 `setx` 报 `无效语法`。

**坑 2：`setx` 后当前窗口不生效**

`setx` 写用户级注册表，**新进程**才读得到。实测确认：写入含空格的值（33 字符）与含分号+冒号的值（41 字符）均完整存储，新进程读回一字不差。

**坑 3：分号在引号内是安全的**

`TAIJI_MCP_SERVERS` 用分号分隔多 server，`TAIJI_MCP_HEADERS_*` 用分号分隔多头——只要整体加引号，`setx` 不会误解析。

### 6.3 查看与删除

```cmd
:: 查看单个
echo %TAIJI_MCP_SERVERS%

:: 列出所有 TAIJI 开头的（PowerShell）
powershell -Command "[Environment]::GetEnvironmentVariables('User').Keys | Where-Object { $_ -like 'TAIJI_*' }"

:: 删除
reg delete HKCU\Environment /F /V TAIJI_MCP_SERVERS
```

---

## 7. 环境变量总表

代码中出现的全部 `TAIJI_` 变量（12 项，来自 `grep -rhno "TAIJI_[A-Z_]*"`）：

| 变量 | 用途 | 类别 |
|---|---|---|
| `TAIJI_MCP_SERVERS` | 挂载的 MCP server 列表 | MCP |
| `TAIJI_ALLOW_TOOLS` | 工具白名单 | MCP |
| `TAIJI_MCP_HEADERS_<名>` | 远程 server 认证头 | MCP（凭据） |
| `TAIJI_MODEL_NAME` | 模型名 | 模型 |
| `TAIJI_MODEL_BASE_URL` | 模型端点（**须自带 `/v1`**） | 模型 |
| `TAIJI_MODEL_API_KEY` | 模型密钥 | 模型（凭据） |
| `TAIJI_WORKSPACE_ID` | 工作区标识 | 路由 |
| `TAIJI_FEISHU_BOT_OPEN_ID` | bot 的 open_id（群聊 @ 判定基准） | 渠道 |
| `TAIJI_FEISHU_ACTIVATION` | 触发模式（`always` / `disabled`） | 渠道 |
| `TAIJI_FEISHU_AUDIENCE` | 受众（`owner_only` 等） | 渠道 |
| `TAIJI_FEISHU_OWNERS` | owner 列表（逗号分隔） | 渠道 |

非 `TAIJI_` 前缀的渠道凭据（`FEISHU_APP_ID`、`FEISHU_APP_SECRET`、`FEISHU_VERIFICATION_TOKEN`、`FEISHU_ENCRYPT_KEY`）见 `internal/config/guard.go` 的 `CredentialKeys`。

**注意**：`TAIJI_MODEL_BASE_URL` 必须自带 `/v1` 路径段——SDK 的拼接方式是 `BaseURL + /chat/completions`（`github.com/openai/openai-go@v1.12.0/internal/requestconfig/requestconfig.go:387`）。

---

## 8. 已知边界

| 项 | 说明 |
|---|---|
| 认证方式 | **仅静态 token/API key**。per-request 动态 token（用户级 OAuth）SDK 支持（`WithMCPOptions` + `mcp.WithHTTPBeforeRequest`），但需从请求上下文传身份，未实现 |
| 远程 MCP 真实服务验证 | 仅本地假 server 的端到端实测；未在 GitHub MCP 等真实托管服务上验证 |
| 退出噪声 | stdio MCP 退出时报 `failed to kill process: invalid argument`（Windows 特有 SDK 噪声），不影响功能 |
| 装配失败行为 | fail-fast（整个进程起不来），非「降级为无工具」 |

---

## 附录 A：本文档的事实来源

| 断言 | 来源 |
|---|---|
| transport 推断规则 | `cmd/taiji/main.go` `mcpTransportForURL` |
| 白名单解析（逗号分隔） | `cmd/taiji/main.go` `envAllowTools` |
| 认证头解析（分号+冒号） | `cmd/taiji/main.go` `mcpHeadersFor` |
| 默认拒绝基线 | `internal/authz/toolpolicy.go:76` |
| 前缀保护 | `internal/config/guard.go` `ReservedPrefixes` |
| 工具名前缀 `{server}_{tool}` | `trpc-agent-go@v1.11.2/internal/tool/toolset.go:251` |
| Headers 到达 HTTP 请求 | `trpc-agent-go@v1.11.2/tool/mcp/toolset.go:293-301`（+ 端到端测试实测） |
| BaseURL 拼接 | `github.com/openai/openai-go@v1.12.0/internal/requestconfig/requestconfig.go:387` |
| 环境变量总表（12 项） | `grep -rhno "TAIJI_[A-Z_]*" --include="*.go" cmd/ internal/` |

## 附录 B：验证记录

| 验证项 | 方法 | 结果 |
|---|---|---|
| 认证头到达 HTTP 请求 | 假 MCP server + httptest | PASS（`Authorization: Bearer e2e-token` 实测到达） |
| 反向验证 | 删除 `mcp.go:95` 的 Headers 透传 | FAIL（`Authorization = ""`）—— 证明测试非空转 |
| CLI 路径读环境变量 | 真实启动 + 观测输出 | `[mcp] mockmcp 已就绪` + `工具策略：默认拒绝，放行 [mockmcp_echo]` |
| 长连接路径读环境变量 | 真实启动（真实飞书凭据） | `[pipeline] MCP server "mockmcp" 已就绪（stdio）` + 白名单行 |
| serve 路径工具执行 | serve 装配参数（`AppName=taiji`, `UserID=feishu`）+ 真实模型 | 回答 = `"Echo: 你好"`（工具真实返回值） |
| `setx` 值存储 | 写入含空格/分号的值，新进程读回 | 33 字符 / 41 字符均完整保留 |
| 全量测试 | `go test ./... -count=1` | 9 包全绿，exit=0 |
