# Taiji · 太极

> Go 原生 Agent Harness —— 以 trpc-agent-go 的工程形态为基础，借鉴 dsh 的会话日志/事件溯源与 capability seam 理念，引入 Tianshu-harness 的认知治理层，并以 happyclaw 的 IM 渠道抽象实现渠道接入与权限管控（飞书优先）。

当前阶段：**已实现可运行原型**（11 个包 / 514 个测试 / 88 次提交）。
支持 LLM 调用、MCP 工具、RBAC 权限、飞书长连接（单/多 agent）。

详细资料见 `docs/` 目录。

## 快速开始（Windows）

前置：Go 1.27.1+、一个飞书自建应用、一个 OpenAI 兼容的 LLM 端点。

```powershell
cd E:\aiops\TaiJi
go build -o taiji.exe ./cmd/taiji

# ── 模型（必填）──
$env:TAIJI_MODEL_NAME     = "deepseek-chat"
$env:TAIJI_MODEL_API_KEY  = "sk-xxxxxxxx"
$env:TAIJI_MODEL_BASE_URL = "https://api.example.com/v1"   # ⚠ 必须自带 /v1

# ── 飞书（必填）──
# agent 与凭据的唯一入口；单 agent 写一条。
$env:TAIJI_AGENTS = "name=assistant,app_id=cli_xxxxxxxxxxxx,app_secret=xxxxxxxxxxxxxxxx"

.\taiji.exe serve --feishu-mode=longconn
```

> bot 的 `open_id`（群聊 @ 判定用）启动时**自动获取**，
> 无需手填——除非自动获取失败（单 agent 可回退 `TAIJI_FEISHU_BOT_OPEN_ID`）。

**完整部署指引（含排障）**：`docs/15-Windows本地开发部署.md`
**环境变量清单**：`docs/13-环境变量清单.md`
**架构与生效范围**：`docs/14-全局设计文档.md`

## 本地联调（不接真飞书）

```powershell
go run ./cmd/taiji chat --mcp "mock=.\mockmcp.exe" --allow-tool mock_echo --debug
```

## 子命令

| 命令 | 用途 |
|---|---|
| `taiji chat` | 交互式对话（本地终端，不经飞书） |
| `taiji serve` | 启动渠道服务（飞书**长连接**，只需出网、无需公网入口） |

> **webhook 接入形态已移除**——`--feishu-mode` 只接受 `longconn`。

## 内置命令（在飞书里发）

| 命令 | 用途 |
|---|---|
| `/help` | 命令清单 |
| `/whoami` | 查看自己的主体 ID（**无需权限**，用于配置 RBAC） |
| `/status` | 会话状态 |
| `/clear` | 开始新会话（历史隔离） |
| `/stop` | 中断当前生成 |

## 文档索引

| 文档 | 内容 |
|---|---|
| docs/01-需求文档.md | 背景与参照系、目标与非目标、功能/非功能需求、关键约束、验收标准 |
| docs/02-设计文档.md | 五层架构、内核/事实/Agent/编排层设计、数据流、接口汇总、风险 |
| docs/03-原型设计文档.md | v1.4 设计的可运行子集（v0.1 原型） |
| docs/04-MCP配置手册.md | MCP server 接入与认证 |
| docs/05~06-飞书卡片流式 | 需求与设计 |
| docs/07-RBAC权限设计.md | 权限模型 |
| docs/08~09-命令系统 | 需求与 SPEC |
| docs/10-Skill支持设计.md | skill 接入 |
| docs/11-多agent与多飞书应用.md | 多 agent 形态 |
| docs/12-agent隔离与父子关系.md | 隔离能力与**实测故障记录** |
| docs/13-环境变量清单.md | 全部环境变量（附读取点） |
| docs/14-全局设计文档.md | 八大主题的全局视图（附 file:line） |
| docs/15-Windows本地开发部署.md | 本地部署操作指南（含排障） |

01/02 均基于四个参照系项目的**源码直读**：deepseek-harness（dsh）、happyclaw、Tianshu-harness、trpc-agent-go。所有设计断言附 `file:line` 证据索引。

## 测试

```powershell
go test ./... -count=1        # 全量（11 个包）
```
