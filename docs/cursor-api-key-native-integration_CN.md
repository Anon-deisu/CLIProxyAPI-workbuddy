# Cursor API Key 管理改版：融入原生管理页路由（去掉浮动按钮）

## 一、背景与目标

此前 Cursor API Key 的管理入口是一个独立的**左上角悬浮紫色按钮**，与原版管理面板的视觉语言格格不入。用户要求将其彻底融入管理面板内部，作为面板的一个原生页面（路由）呈现：

- 目标页面：`https://ovh.xem8k5.top:9443/management.html#/oauth` 所在的同一套面板设计体系。
- 新路由：`#/cursor-api-key`，Nav 侧边栏中紧跟在 `/oauth` 之后新增一项。

## 二、实现方式

管理页 `management.html` 保持**下载原版字节不变**，采用**服务端内存打补丁**策略，在 serve 时实时注入（因为 `management.html` 每次请求都被重新读取并加工）：

- 入口：`internal/api/server.go` 中 serve management.html 时依次调用
  `managementasset.AddCursorOAuthToManagementHTML(data)`（原有）与
  `managementasset.AddCursorAPIKeyManagerToManagementHTML(data)`（新增）。
- **函数名与调用位置不变**，`server.go` 无需改动 —— 只在 `internal/managementasset/cursor_api_key.go` 内重写。

### 注入的四处内容

1. **导航项**：锚定 `icon:K9.oauth}`，在其后追加 `,{path:`/cursor-api-key`,labelKey:`nav.cursor_api_key`,metaKey:`nav_meta.cursor_api_key`,icon:K9.oauth}`。
2. **路由项**：锚定 `(0,H.jsx)(VM,{})}`（route 对象后的唯一标记，注意含结尾 `}`），在其后追加 `,{path:`/cursor-api-key`,element:(0,H.jsx)("div",{id:"cpa-native-route-host"})}` —— 用 JSX 冒号属性语法创建承载 div。
3. **语言标签**：在 8 个固定锚点逐一插入 `cursor_api_key:`Cursor API Key`,`（锚点形如 `oauth:`...`,quota_management:`）；另有正则兜底，对任意 `oauth:`<值>`,quota_management:` 都可插入。
4. **管理脚本**：`</body>`（或 `</html>`、或文件末尾）前注入一个 IIFE 脚本，负责认证捕获（XHR/fetch 对 `/v0/management/` 路径的 monkey-patch）、Key 列表/新增/编辑/删除、错误展示等原生交互。

### 幂等性（关键）

补丁函数开头用 `const cursorAPIKeyManagerMarker = \`id:"cpa-native-route-host"\`` 做幂等守卫：若内容已含该标记则直接返回原数据，**不重复注入**。

> ⚠️ **标记必须用 JSX 冒号形式 `id:"..."`（0x3a），而非 HTML 等号形式 `id="..."`（0x3d）。**
> 因为注入的路由 div 是 JSX 语法 `{id:"cpa-native-route-host"}`。若用等号形式，幂等检查永远无法命中，每次 serve 都会重复注入（历史上正是这个 bug 导致重复注入）。

## 三、本次修复记录（提交 0ba06b46）

| 变更 | 说明 |
|------|------|
| `cursor_api_key.go` 第 11 行 | 幂等标记从 `id="..."`（等号）改为 `id:"..."`（冒号），与注入 JSX 完全一致 |
| `cursor_api_key_test.go` | 重写/新增原生标记、锚点、脚本、幂等等断言 |
| `server_cursor_keys_test.go` | 废弃的浮动按钮测试重写为 `TestManagementPageIntegratesCursorEntryAndDisablesCaching`：断言原生集成出现 + 浮动按钮消失 + 两次请求幂等 + `Cache-Control: no-store` |
| `server_test.go`（无关） | 顺带修正陈旧断言 `TestModelsWithClientVersionReturnsCodexCatalog`（custom priority 129→143，嵌入式模板 base priority 已从 29 变更为 43，见代码公式 `base + 100*(rank+1)`） |

`TestCursorAPIKeyManagementRouteMasksSecret` 保持通过（secret 掩码为 `crsr_********cret`）。

## 四、部署记录

- **本地**：`CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o ... ./cmd/server`（主程序在 `cmd/server`，非仓库根目录）。
- **校验**：scp 上传后 `sha1sum` 比对一致（`1964fb0fd4c85c019b1a14ec9b1841b325e20e08`）。
- **原子替换**：先 `cp /usr/local/bin/cli-proxy-api <...>.bak`，再 `mv` 新二进制入位，`chmod 755`，`systemctl restart cli-proxy-api`。
- **确认接入流量后才清理备份**：healthz 200 + 进程为新 PID（启动 13s）+ 线上管理页实测包含 `cpa-native-route-host`、路由 `/cursor-api-key`、浮动按钮样式计数为 0；公网 `https://ovh.xem8k5.top:9443/management.html` 复测通过后，删除 `.bak`。
- **最终状态**：`systemctl is-active cli-proxy-api` → `active`；配置 `debug: false`（生产要求保持）。

## 五、验证命令（可复用）

```bash
# 线上管理页是否已注入原生 Cursor Key 路由
curl -s http://127.0.0.1:8317/management.html | grep -c "cpa-native-route-host"

# 导航路由是否存在
curl -s http://127.0.0.1:8317/management.html | grep -o 'path:`/cursor-api-key`'

# 浮动紫色按钮是否已彻底消失（计数应为 0）
curl -s http://127.0.0.1:8317/management.html | grep -c ":host{position:fixed;inset:0;z-index:2147482999"
```
