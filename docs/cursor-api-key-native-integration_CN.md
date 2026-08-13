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

### 迁移：初次启动自动导入旧配置的 key 并删除旧字段（Task #8）

旧独立页面把凭据持久化到 **config.yaml 的 `cursor-api-key` 块**（`h.cfg.CursorKey = ...; h.persistLocked(c)`），从未写 auth 目录 JSON。生产服务器 `/etc/cli-proxy-api/config.yaml` 上**实际遗留了 3 个在用 key**（`crsr_9df03fee…` 等）。新二进制删除了 `config.CursorKey` 字段、对该块直接忽略，因此若不迁移，这些 key 将彻底不可达——**这是一个真实的、必须做的迁移**。

`internal/auth/cursor/legacy_migrate.go` 实现了幂等的一次性迁移，由 `NewServer` 在每次启动时调用（`server.go`）：

1. 读取 config.yaml 顶层 `cursor-api-key:` 节点（用 `yaml.Node` 判定**块是否存在**——含空块 `cursor-api-key: []`，区别于块缺失）。
2. 对每个旧 key：若其 `base_url+api_key` 已存在于 JSON store（防重复），跳过；否则用 store 的 `Create` 落成标准 JSON 文件（`<…>_cursor-api-key.json`）——走同一套原子写 + fsnotify，watcher 立即重新分发。
3. 迁移完成后 `stripLegacyCursorAPIKeyBlock` 以 **yaml.Node 编辑 + 原子写** 删除 config.yaml 中的 `cursor-api-key` 块，**保留其余注释与节**。
4. 幂等：块缺失则完全不改写 config.yaml；重复 key 去重；出错（无法读/写/单条失败）只记日志、绝不阻塞启动，留待下次启动重试。

- **不再写 config.yaml 的 `cursor-api-key`**：`config.CursorKey` 字段连同该块已完全删除（`removeRemovedIntegrationKeys` 也会在后续管理保存时顺带剔除残留块）。
- 迁移成功后，仅从 **auth 目录 JSON 文件**加载；任何已通过新方案保存的 JSON 都会被原样读取，零转换。
- 安全性：迁移在服务端日志中只记文件名与数量，**绝不输出 key 明文**。

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
