# Taiji · 太极

> Go 原生 Agent Harness —— 以 trpc-agent-go 的工程形态为基础，借鉴 dsh 的会话日志/事件溯源与 capability seam 理念，引入 Tianshu-harness 的认知治理层，并以 happyclaw 的 IM 渠道抽象实现渠道接入与权限管控（飞书优先）。

当前阶段：**需求与设计定稿（v1.4.1），尚无代码实现。**

## 文档

| 文档 | 内容 |
|---|---|
| docs/01-需求文档.md | 背景与参照系、目标与非目标、功能/非功能需求、关键约束、验收标准、分期规划 |
| docs/02-设计文档.md | 五层架构、内核/事实/Agent/编排层设计、数据流、接口汇总、风险、六波实施计划 |

两份文档均基于四个参照系项目的**源码直读**：deepseek-harness（dsh）、happyclaw、Tianshu-harness、trpc-agent-go。所有设计断言附 `file:line` 证据索引。
