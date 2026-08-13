# Cursor 上游静默卡死修复（思考期间 Keep-alive + 硬超时兜底）

## 一、问题

用户反馈：**"六分钟，第一分钟吐了 2.1k，后面一个字没吐，直接报完成了"** —— 模型（fable-5，xhigh）长时间思考时，Claude Code 端看到的是 SSE 完全静默，随后在**没有任何正文**的情况下"完成"。

服务器日志证据：一次请求被挂起 **31 分 46 秒**、无任何字节产出。

## 二、根因

两条叠加：

1. **读取侧无空闲/超时检测**。`readLoop` 的 `stream.Read`（Agent H2 `BidiStream.Read`，[h2.go](internal/cursor/h2.go)）不设 deadline，一旦上游（Cursor 的 provider）在长思考期间不产出字节，它会在一个阻塞的 `Read` 上无限挂起——请求生命周期被压到 31 分钟级。

2. **心跳只发给上游，从不转发下游**。`heartbeatLoop` 每 5s 给 Cursor 发心跳保活，但**没有任何控制面事件转发给下游客户端**。Claude Code 因此无从区分"仍在思考"与"连接已死"，本地超时后即按"完成"收尾，正文流自然为空。

> 设计上两个问题都不在翻译层：`handleThinkingDelta` 已把推理**实时**经 `thinking_delta` 流式转发（见 [session.go](internal/cursor/session.go)）。`openai_claude_response.go` 的 `reasoning_content → thinking block` 映射也验证正确。缺的只是"上游静默期给下游一个保活信号 + 兜底超时"。

## 三、修复

### 1. 会话侧：`stallWatcher`（[session.go](internal/cursor/session.go)）

新增两个常量与一个看门狗 goroutine，在 `StartSession` 中与 `heartbeatLoop` 一并启动：

```go
const (
	// 下游可见静默超过该值，转发一次保活 ping
	cursorIdleHeartbeatInterval = 30 * time.Second
	// 硬上限：单段上游静默超过该值，判定死连接并 fail，终止无限挂起
	cursorStallTimeout = 15 * time.Minute
)

func (s *Session) stallWatcher(ctx context.Context) { /* 见源码 */ }
```

行为：
- 每 `cursorIdleHeartbeatInterval` 无新帧 → `s.emit(StreamEvent{Type:"heartbeat"})`（交给执行器转发为下游保活）；
- 连续静默达 `cursorStallTimeout` → `s.fail(...)`，把死 H2 流转为有界错误，不再无限阻塞；
- 唯一退出条件是会话关闭（`s.closed`/ctx 取消/`s.done`）——新帧到达会重置这两个计时，合法收尾不会误触发。

计时基于既有字段 `s.lastFrame`（帧到达时更新，与 `heartbeatLoop` 同一把 `s.mu` 保护），复用同一套同步与语义。

### 2. 执行器侧：映射 heartbeat → SSE 保活（[cursor_executor.go](internal/runtime/executor/cursor_executor.go)）

- 新增 `emitRaw` 助手：把**已翻译好的**帧原样送往下游 `out`，绕过 chunk 翻译器（否则未识别帧会被静默丢弃）。
- `iterate` 切换中新增 `case "heartbeat":`，经 `emitRaw` 写出 SSE **注释行**：

```go
": keep-alive (cursor upstream thinking)\n\n"
```

选择注释行的理由：
- 所有 SSE 解析器（Claude Code 与 OpenAI 兼容客户端）**一律忽略注释行**，绝无注入/解析破坏风险；
- 但它会**重置客户端侧空闲计时**——长时间上层思考不再被读成"冻结流"；
- 兼顾两条下游路线（`/v1/messages` 与 `/v1/chat/completions`），无需区分目标就是 Anthropic `ping` 还是 OpenAI 空帧。

`emitRaw` 若因 ctx 取消写不出去则返回 `context.Canceled`，与 `iterate` 既有中止语义一致。

## 四、验证

- `go build ./...` 通过；
- `go vet ./internal/cursor/ ./internal/runtime/executor/` 无告警；
- `go test ./...` 相关包全绿：`internal/cursor`、`internal/runtime/executor`、全部 `internal/translator/*`。
- （注：`internal/api` 的 `TestModelsWithClientVersionReturnsCodexCatalog` 失败与本次改动无关——那是 codex 模型目录优先级断言的历史值漂移，属待办合并工作，非本修复引入。）

## 五、部署记录（2026-08-13 ~ 08-14）

- 提交：`2d9b2f10`（`fix(cursor): surface long upstream thinking as keep-alive, bound stalls`），已推送分支 `modeb-relay`。
- 生产：`root@ovh.xem8k5.top`，systemd 单元 `cli-proxy-api.service`，二进制 `/usr/local/bin/cli-proxy-api`，config `/etc/cli-proxy-api/config.yaml`。
- 本地静态交叉编译：`CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" ./cmd/server`。
- 新二进制 md5 `61f7de56a7fa5c3cf855f356204586c4`（57995426 字节）；旧版 `.bak-latest` = `84ad66aefc30bdece420d344c1885109`。
- atomic 替换：`install -m 0755` 覆盖 → `systemctl restart` → `systemctl is-active`=active，`NRestarts=0`，`MainPID=2076397`。
- 新程序自启动即持续收到 `200 POST /v1/messages` 与 `/v1/chat/completions` 请求（来源 IP `95.169.7.75`、`10.200.2.128`），0 错误、0 panic，**流量确认无误**。
- 已清除本轮 `.bak-latest` 备份；服务器现仅存运行中的新二进制与 `config.yaml`。

> 备份（`.bak-latest` / `.bak-prev` / `config.yaml.bak-debug`）遵循"确认新二进制持续接收流量后清除"原则；未确认前绝不删除。

## 六、实测观察要点（部署后）

- 长思考请求（高性能/高推理档）在静默段应能看到下游收到 `: keep-alive` 注释行（`debug: true` 时日志出现 `cursor: upstream idle ...; sending downstream heartbeat`）；
- 若真出现死连接，15 分钟后应看到 `cursor: upstream stalled ...; failing session`，后端先于客户端超时终止，而不是客户端"无声完成"。
