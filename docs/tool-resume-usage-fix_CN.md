# 工具续接 789K 输入 / 0 输出 缓存计数异常 · 根因与修复文档

- **编写日期**：2026-08-13
- **涉及提交**：`f541c5eb`（`fix(cursor): 移除工具续接重建时 conversation id 轮换`）
- **涉及模块**：`internal/runtime/executor/cursor_executor.go`、`internal/cursor/{session.go, session_manager.go, conversation_continuation.go}`（后三者仅诊断日志）
- **影响范围**：Cursor 协议下的工具续接（tool-resume）路径

---

## 一、症状

生产环境 `usage` 统计出现严重异常：工具调用续接时，单请求被统计为 **输入 ~789K tokens、输出 0 tokens**（789K 量级随历史长度在 150 ~ 1133 tokens × N 之间浮动）。表现为：

- 缓存计数严重偏高，计费失真；
- 工具续接请求实际没有产生任何输出内容，却把整段历史当作输入重新结算。

## 二、根因（已完全确认）

### 1. 直接触发点：工具续接失败 → 全历史重建

- 工具执行循环返回 `ErrToolResumeRebuild` → 会话被 `session.Close()` 悬空为孤儿 → 硬失败 → 走**全历史重建路径** `buildRunRequestForConversation`。
- 重建路径 `/v1/messages` 请求体构造见 [agent.go:288-313](internal/cursor/agent.go)：
  - 无活跃用户消息时发送**空 `ResumeAction{}`**；
  - 完整历史被重渲染进 `RootPromptMessagesJson` blob + `PreFetchedBlobs` 一次性投递。
- **每次工具续接都重渲染全部历史**，于是输入 token = 全历史总和，输出 = 0。这是 789K 数字的直接来源。

### 2. 放大因素：conversation id 被轮换（本次修复点）

- 修复前的重建路径每次调用都追加 `uuid.NewString()` 轮换 conversation id（见 [cursor_executor.go](internal/runtime/executor/cursor_executor.go)）。
- 每次轮换都会：
  1. **孤立 Cursor 服务器端会话状态**（含 pending 工具调用），使服务器不得不重新处理全部历史；
  2. **击穿续接快照查询** `takeContinuation(conversationID)`，导致 `StartSession` dispatch（[session.go:247-277](internal/cursor/session.go)）永远走不到 `buildContinuationRunRequest`（UserMessageAction 增量续接）；
  3. 重新制造同样的 789K 输入 / 0 输出成本。

### 3. 为什么工具结果只能重渲染（Proto 限制，决定性）

独立重读编译后 proto：[agent_mvp.pb.go:333-347](internal/cursor/proto/agent/v1/agent_mvp.pb.go)。

- `isConversationAction_Action` 接口只有**两个** oneof 实现：
  - `ConversationAction_UserMessageAction`（含 `UserMessage`）；
  - `ConversationAction_ResumeAction`。
- **没有任何工具结果 / 增量内容 action 变体**。
- 结论：工具结果在协议层**无法作为最小 action 投递**，只能作为重渲染历史 blob 传达。因此工具续接路径下"完整历史重定价"是**协议固有且不可避免**的——只要能表达工具结果，就必须带上整段历史。

> 此结论推翻了"Phase 2：引入 `tool_resume_trailer` 配置项、用最小 trailer `ResumeAction` 表达工具续接"的早期设计。该设计在当前 proto 下不可实现，正式取消，**不引入该 flag**。

## 三、Phase 1 修复（唯一可实现且正确的修复）

工具续接重建路径**保持 conversation id 确定性稳定**，不再轮换：

- `resolveCursorConversationID` / `scopeCursorConversationID` 使用作用域 uuid 的 **SHA1** 生成确定性 id，在相同作用域内始终稳定指向同一个会话；
- 移除 `openCursorSession` 中 `_ = session.Close()` 后追加 `uuid.NewString()` 的 `WithConversationID` 轮换调用（[cursor_executor.go](internal/runtime/executor/cursor_executor.go) 466 行起）。

修复收益：

1. **正确定位孤立会话**：工具续接定位到服务器端同一会话，不孤立 pending 工具调用；
2. **恢复文本回合增量续接**：干净的文本回合命中 `takeContinuation` / `continuationMatchesHistory` → 走 `buildContinuationRunRequest`（UserMessageAction 增量），不再全量重建；
3. **收敛成本**：只有真正的工具续接失败才全量重建（此时 789K 是协议固有代价），正常文本回合不再每次付出该代价。

## 四、诊断日志（保留在发布构建，零运行时开销）

三个文件新增的日志全部为 `log.Debugf`，在 `debug: false` 时惰性失效：

| 文件 | 内容 |
|------|------|
| [conversation_continuation.go](internal/cursor/conversation_continuation.go) | `continuationMatchesHistory` 各拒绝原因的 `logFail` 日志 + `truncateForLog` helper |
| [session.go](internal/cursor/session.go) | `rebindPendingLocked` 的 wasEmitted/oldIDs 追踪，`rebind-EMITTED` / `rebind pre-emit` 日志；`emitPendingToolBatch` / `emitNextPendingTool` 的 emit tool_call 日志；`PendingCallIDAlive` 方法 |
| [session_manager.go](internal/cursor/session_manager.go) | `scanSessionsForPendingLocked`：返回 pending tool_call_id 在哪些会话仍被索引 |

若 789K 症状在真实流量下复现：将生产 `/etc/cli-proxy-api/config.yaml` 的 `debug: true`，即可获得 id 空间 / pending 分叉 / continuation 拒绝原因诊断。

## 五、部署记录（2026-08-13）

- 生产：`root@ovh.xem8k5.top`，systemd 单元 `cli-proxy-api.service`，二进制 `/usr/local/bin/cli-proxy-api`，config `/etc/cli-proxy-api/config.yaml`。
- 本地静态交叉编译：`CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w"`（OVH VPS 无 Go 工具链）。
- 部署方式：**atomic systemd 替换**（两代备份链 `.bak-latest` / `.bak-prev`），生产 config 重置 `debug: false`。
- 部署后确认：`systemctl is-active` = active，`NRestarts=0`，日志见正常 `200 POST /v1/messages` 流量；工具续接失败时日志出现预期行
  `cursor: stream resume tool session failed (...); rebuilding turn from history`（走重建路径为预期行为，新构建已无 conversation id 轮换）。

### 部署后验证

| 项 | 值 |
|------|-----|
| 运行中 `/usr/local/bin/cli-proxy-api` md5 | `4a62620c84fdb4cb999d3127a228e5f3`（新构建，58MB） |
| 部署前运行版 md5 | `77031607` 字节体积（旧构建，77MB） |
| 服务 PID / 起始 | `776684` / `2026-08-13 15:27:51 UTC` |
| `NRestarts` | 0（部署后无崩溃重启） |
| 流量核对 | 部署后日志持续出现 `200 POST /v1/messages`，来源 `95.169.7.75` / `10.200.2.128` |

> 备份（`.bak-latest` / `.bak-prev` / `config.yaml.bak-debug`）已在**确认新二进制持续接收流量**后清除。

## 六、备份清理

本次部署确认新二进制持续接收流量后（见上表），已清除全部 `.bak` 备份：

```bash
rm -f /usr/local/bin/cli-proxy-api.bak-latest /usr/local/bin/cli-proxy-api.bak-prev \
      /etc/cli-proxy-api/config.yaml.bak-debug
```

> **原则：备份只在确认新版本已正常接收流量之后才删除。未确认前绝不删除。**
>
> 若需要回滚到上一发布版本：需先将要恢复的历史镜像重新部署（或从 git 重新构建 `CGO_ENABLED=0 GOOS=linux GOARCH=amd64`）再 `install` 覆盖运行 `cli-proxy-api` 后 `systemctl restart`。备份本身不再长期驻留服务器。

## 七、遗留

- 工具续接失败时协议固有的一次全量重定价（789K 类）无法在本 proto 下消除；这是协议约束，不是缺陷。若未来 proto 增加工具结果 action 变体，即可实现真正的 Phase 2（最小 trailer 续接）。

---

# 追加修复：子代理 `model` 参数校验失败（InputValidationError）

- **编写日期**：2026-08-13
- **涉及提交**：待提交（当前工作区）
- **涉及模块**：[session.go](internal/cursor/session.go)（`mapNativeExecArgs` 的 `subagent_args` 分支 + 新增 `claudeCodeModelName`）/ `claude_code_model_test.go`（新增单测）
- **影响范围**：Cursor 协议下模型调用 `Agent`/`Task`/`Subagent` 子代理工具时

## 一、症状

`tool_use_error` 报 `InputValidationError: Invalid option: expected one of "sonnet"|"opus"|"haiku"|"fable"`，出错点在子代理工具的 `model` 参数路径上。

## 二、根因

- Claude Code 的 `Agent`/`Task`/`Subagent` 工具，其 `model` 参数**只接受家族缩写**：`sonnet`、`opus`、`haiku`、`fable`。
- Cursor 的 `SubagentArgs.ModelId`（proto [agent_mvp.pb.go:7203](internal/cursor/proto/agent/v1/agent_mvp.pb.go)，wire 字段 3）携带的是 **Cursor 内部 wire id**（例如 `cursor-claude-4.6-sonnet`）。
- 修复前 [session.go:3529-3531](internal/cursor/session.go) 直接写 `out["model"] = args.GetModelId()`，把 Cursor 内部 id 原样透传给 Claude Code → 校验失败 → 整个子代理工具调用被拒。

## 三、修复

新增 `claudeCodeModelName` 映射函数：把 Cursor wire id 小写化、按非字母数字边界切分词元，命中 `sonnet`/`opus`/`haiku`/`fable` 中任一家族 token 即返回对应缩写（`cursor-claude-opus-5-fast` → `opus`）。**未命中任何家族时返回空串**，调用方随之**省略 `model` 字段**，让 Claude Code 走默认模型，而不是让整个工具调用失败。

```go
// session.go:subagent_args 分支
if model := claudeCodeModelName(args.GetModelId()); model != "" {
    out["model"] = model
}
```

边界处理：

- 非 Claude 模型（`gpt-5.4`）、未知家族 → 省略 `model`（降级为默认），绝不传非法值；
- 嵌入式假阳性（`sonsonnet`、`myopus-v3`）→ 因要求独立 token 而正确不匹配；
- case-insensitive（`Cursor-Claude-4.6-Sonnet` → `sonnet`）。

单测：`TestClaudeCodeModelName`（[claude_code_model_test.go](internal/cursor/claude_code_model_test.go)）覆盖以上 13 个用例，全部通过；`go vet ./internal/cursor/` 无告警。

## 四、部署记录（追加，2026-08-13 15:53 UTC）

- 沿同一 atomic systemd 替换流程部署到 `root@ovh.xem8k5.top`，分支 `modeb-relay`，生产 config `debug: false`。
- 本地静态交叉编译：`CGO_ENABLED=0 GOOS=linux GOARCH=amd64`（OVH VPS 无 Go 工具链）。
- 提交：`52eb5b44`（`fix(cursor): 子代理工具 model 参数映射为 Claude Code 家族缩写`），已推送。
- 新二进制 md5 `84ad66aefc30bdece420d344c1885109`（57995426 字节），`systemctl is-active` = `active`，`NRestarts=0`，`MainPID=1054674`。
- 新程序自启动后即持续收到 `200 POST /v1/messages` 请求（来源 IP `95.169.7.75`、`10.200.2.128` 等），**流量确认无误**。
- 清除本轮部署产生的服务器端备份：`/usr/local/bin/cli-proxy-api.bak-latest` 已删除；`.bak-prev`、`config.yaml.bak-debug` 本轮未产生，无需处理。服务器现仅存运行中的新二进制与 `config.yaml`。

> 备份（`.bak-latest` / `.bak-prev` / `config.yaml.bak-debug`）同样遵循"确认新二进制持续接收流量后清除"原则；未确认前绝不删除。未触碰的备份绝不删除。
