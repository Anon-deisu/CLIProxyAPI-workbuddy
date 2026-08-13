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

### 部署后 md5 校验

| 路径 | md5 |
|------|-----|
| 运行中 `/usr/local/bin/cli-proxy-api` | `4a62620c84fdb4cb999d3127a228e5f3` |
| `/usr/local/bin/cli-proxy-api.bak-latest` | `1619efc85e5380901545adfe42b84288`（部署前运行版本） |
| `/usr/local/bin/cli-proxy-api.bak-prev` | （与 `.bak-latest` 相同，见回滚说明） |
| 服务 PID / 起始 | `776684` / `2026-08-13 15:27:51 UTC` |

## 六、回滚与备份清理

### 回滚（如新版本异常）

```bash
# 恢复上一代为运行版本
install -m 0755 /usr/local/bin/cli-proxy-api.bak-latest /usr/local/bin/cli-proxy-api
systemctl restart cli-proxy-api
```

### 备份清理（仅在确认流量已正常接收后执行）

```bash
rm -f /usr/local/bin/cli-proxy-api.bak-latest /usr/local/bin/cli-proxy-api.bak-prev \
      /etc/cli-proxy-api/config.yaml.bak-debug
```

> 清理必须遵循"确认新二进制已接收流量"这一前提 —— 只有日志确认存在正常请求记录后才删除备份。**未确认前绝不删除。**

## 七、遗留

- 工具续接失败时协议固有的一次全量重定价（789K 类）无法在本 proto 下消除；这是协议约束，不是缺陷。若未来 proto 增加工具结果 action 变体，即可实现真正的 Phase 2（最小 trailer 续接）。
