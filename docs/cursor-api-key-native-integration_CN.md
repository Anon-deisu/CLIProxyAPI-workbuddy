# Cursor API Key 管理：卡片内嵌输入 + JSON 存储（移除独立页面）

## 一、背景与目标

Cursor API Key 的凭据管理经历了三轮演进，最终收敛为「去独立页面 + 统一 JSON 存储」：

1. **旧版**：独立 `#/cursor-api-key` 页面 + 左上角悬浮按钮，凭据通过 `config.yaml` 的 `cursor-api-key` 字段持久化。
2. **中断版**：独立页面仍在，但已尝试把凭据落到 JSON 文件。
3. **现行（本方案）**：
   - **完全删除独立 `#/cursor-api-key` 页面**（route hook、nav 项、i18n 标签一并移除）；
   - 在 `#/oauth` 的 **Cursor OAuth 卡片内**嵌入一个**内联输入框**（卡片内嵌输入框），用于快速添加/查看 Key；
   - 凭据统一在 `#/auth-files` 下新增的独立 **「Cursor API Key」** 分类管理；
   - **存储**：彻底放弃 `config.yaml` 的 `cursor-api-key` 字段（`cursor-api-key` 污染），只从 **auth 目录下逐个 JSON 文件**加载；`config.CursorKey` 字段完全删除。

## 二、存储方案（JSON Store）

### 文件位置

每个 Cursor API Key 独立成一个 JSON 文件，位于配置的 `auth` 目录（`/var/lib/cli-proxy-api/auth`）下：

```
auth/<email>_cursor-api-key.json
```

- 文件名 `<email>_cursor-api-key.json`（email 含特殊字符时退化为 base URL 的 sha256 摘要，再退化为 `cursor-api-key`）。
- 目录内文件按字典序排序，**稳定索引**用于增删改。

### JSON Schema

```json
{
  "type": "cursor-api-key",
  "api_key": "crsr_xxxx",
  "priority": 100,
  "prefix": "crsr_",
  "base_url": "https://api2.cursor.sh",
  "excluded_models": ["gpt-auto"],
  "disable_cooling": false,
  "disabled": false,
  "note": "",
  "email": "user@example.com",
  "auth_kind": "api-key",
  "created_at": "2026-08-14T04:00:00Z",
  "updated_at": "2026-08-14T04:00:00Z"
}
```

### 后端 Store

- `internal/auth/cursor/api_key_store.go` —— `APIKeyStore`：`List / Get(index) / Create(rec) / Update(index) / Delete(index)`。
- `internal/auth/cursor/api_key_storage.go` —— `APIKeyStorage`：自描述 JSON schema、`ResolvedBaseURL()`、`IsCursorAPIKeyRecord(rec)`（按 `type` 判别式判断）。
- **原子写入**：`writeRecord` 先写临时文件再 `os.Rename`，fsnotify 永远不会观察到半截 JSON。
- `cursorKeyStore()`（`config_cursor_keys.go`）= `cursor.NewAPIKeyStore(cfg.AuthDir)`，根目录即配置的 auth 目录。
- watcher 的 `synthesizeCursorAPIKey`（`internal/watcher/synthesizer/file.go`）把 JSON 文件实时合成为 Auth 对象，与既有 `#/auth-files` 走同一套发现机制。

### 迁移结论（Task #8）

**无需迁移**。旧独立页面把凭据写进 `config.yaml` 的 `cursor-api-key`（`h.cfg.CursorKey = ...; h.persistLocked(c)`），从未写过 auth 目录下的 JSON 文件。因此：

- auth 目录中**不存在旧方案遗留的** `*_cursor-api-key.json` 需要导入；
- `config.CursorKey` 字段连同 `cursor-api-key` 配置污染已完全删除，新二进制对旧配置中的该块**直接忽略**；
- 任何已通过新方案保存的 JSON 文件都会被新 store 原样读取，零转换。

## 三、管理界面

### OAuth 卡片内嵌输入（`AddCursorAPIKeyInlineToManagementHTML`）

`internal/managementasset/cursor_api_key.go` 在 serve `management.html` 时注入一段 IIFE 脚本（`<script id="cpa-key-inline-script">`）：

- 用 `MutationObserver` **扫描文档中的 Cursor OAuth provider 卡片**，在卡片内嵌入一个列表 + 内联输入框（填 Key → 「添加」）;
- 通过 `managementPath = '/v0/management/'` + `cursor-api-key` 拼接端点（**连续字面量 `/v0/management/cursor-api-key` 不会出现在脚本里**）；
- 状态提示：「共 N 个 Key；完整编辑请前往「Auth Files → Cursor API Key」」；
- 认证复用已登录面板自动捕获的 `Authorization: Bearer`，无手工填表入口。

### 幂等性（关键，生产 bug 修复）

```go
const cursorAPIKeyInlineMarker = `id="cpa-key-inline-script"`
```

`AddCursorAPIKeyInlineToManagementHTML` 开头的幂等守卫 `bytes.Contains(data, marker)` 判定页面是否已被注入过。

> ⚠️ **标记必须用 HTML 等号 `id=`（0x3d）**，与注入出的 `<script id=...>` 完全一致。
> 若误写成 `id:`（冒号，JSX 风格），`bytes.Contains` 永远无法命中，**每次 serve 都会重复注入**——这就是历史上导致重复注入的生产 bug（`cursor_api_key.go:11` 已修复）。

## 四、HTTP 管理端点

`internal/api/server.go:743-747` 注册 5 个端点：

| 方法 | 路径 | Handler |
|------|------|---------|
| GET | `/v0/management/cursor-api-key` | `GetCursorKeys`（不暴露明文凭据） |
| POST | `/v0/management/cursor-api-key` | `PostCursorKey`（追加一个） |
| PUT | `/v0/management/cursor-api-key` | `PutCursorKeys`（整体替换） |
| PATCH | `/v0/management/cursor-api-key` | `PatchCursorKey`（按索引改） |
| DELETE | `/v0/management/cursor-api-key` | `DeleteCursorKey`（按索引删） |

## 五、验证命令（可复用）

```bash
# 幂等标记只注入一次（计数应为 1）
curl -s http://127.0.0.1:8317/management.html | grep -c 'id="cpa-key-inline-script"'

# 连续字面量 /v0/management/cursor-api-key 不得出现在脚本里（计数应为 0），
# 端点经由 managementPath + 'cursor-api-key' 拼接
curl -s http://127.0.0.1:8317/management.html | grep -c 'managementPath = .\/v0\/management\/.'

# 确认已无旧独立页面路由（计数应为 0）
curl -s http://127.0.0.1:8317/management.html | grep -c 'path:`/cursor-api-key`'

# 确认 no-store（管理页不缓存）
curl -sI http://127.0.0.1:8317/management.html | grep -i 'cache-control'
```

## 六、部署记录

- **本地构建**：`CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o ... ./cmd/server`（主程序在 `cmd/server`，非仓库根目录）。
- **原子替换**：先 `cp /usr/local/bin/cli-proxy-api <...>.bak`，再 `mv` 新二进制入位，`chmod 755`，`systemctl restart cli-proxy-api`。
- **确认接入流量后才清理备份**：healthz 200 + 进程为新 PID + 线上管理页实测包含 `id="cpa-key-inline-script"`（计数 1），公网复核通过后**才删除** `.bak`。
- **生产要求**：`debug: false` 保持；`systemctl is-active cli-proxy-api` → `active`。
