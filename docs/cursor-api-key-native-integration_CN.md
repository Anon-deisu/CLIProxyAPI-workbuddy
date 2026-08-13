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

### 修复记录 1：管理页空白（提交 13b459ff）

面板路由 `#/cursor-api-key` 打开时白屏，控制台报 `Cannot read properties of null (reading 'addEventListener')`（`management.html:432`）。

- **根因**：`list.addEventListener(...)` / `form.addEventListener(...)` 在 DOM 尚未构造完成前就执行，`panel.querySelector('#cpa-key-list')` / `#cpa-form` 返回 `null`。
- **修复**：先在脚本末尾统一 `querySelector` 取回全部元素，**再**挂事件监听（监听器挂载严格放在 DOM 构造之后）。
- **部署二进制**：`a352b8d62dbb154647ab119c79938238f830c7da0b4f2e420463f026d98f1ed6`。

### 修复记录 2：移除手动「管理密钥」输入框（提交 65287d49）

用户已在管理面板登录，但 `#/cursor-api-key` 页顶部仍显示一个割裂的**手动凭据输入框**「管理密钥（通常自动获取）」。要求：已登录时该框**完全不出现**，页面与 `#/oauth` 浑然一体，且通过同一套已登录的管理会话（`#/auth-files` 机制）工作。

- **根因**：旧实现把「管理密钥」做成手动输入框兜底，破坏了已验证会话的复用。
- **修复**（`internal/managementasset/cursor_api_key.go`）：
  - 删除整个「管理凭据」卡片（`label for="cpa-management-key"`、`input#cpa-management-key`、提示文案），并移除 `managementKeyInput` 声明与其 DOM 查询。
  - `currentManagementKey()` 改为仅返回 `capturedManagementKey || rememberedManagementKey()` —— 管理密钥完全来自**自动捕获的 `Authorization: Bearer` 头**（对 `/v0/management/` 的 XHR/fetch monkey-patch）或 `localStorage['cli-proxy-auth']`，不再有任何手工填写入口。
  - `request()` 错误提示改为「尚未获取到管理密钥。请确认已登录管理面板，然后刷新页面重试。」
  - `load()` 新增一次性 600ms 重试：覆盖**直接深链进入** `#/cursor-api-key` 的时序（此时面板自身的认证请求尚未发出，密钥暂缺；600ms 后面板已带 `Authorization` 头发起管理请求并被自动捕获）。
  - 回归测试 `cursor_api_key_test.go` 新增「gone」断言循环：打补丁后的输出不得再出现 `cpa-management-key` / `管理密钥（通常自动获取）` / `managementKeyInput` / `在下方的管理密钥框中临时填写`。
- **线上确认**（`/management.html`，非 `/static/management.html`）：`cpa-management-key` 计数 0、`仅在自动获取失败时填写` 0、`不会写入配置或浏览器永久存储` 0；`cursorAPIKeyManagerMarker` 计数 2、新提示语「尚未获取到管理密钥」出现 1 次、`#cpa-refresh`/`#cpa-add` 存在、`MutationObserver` 4 次、`OAuthPage provider section` 标题 4 处（oauth + cursor-api-key + auth-files + …均正常，oauth 集成未受影响）。
- **部署二进制**：`5771d3ee2935b218ed663ec5b9e8403191c64439d52935f3a6457bf5bc651e26`（`sha256`；`debug: false` 保持，`systemctl is-active` → `active`）。

## 五、验证命令（可复用）

```bash
# 线上管理页是否已注入原生 Cursor Key 路由
curl -s http://127.0.0.1:8317/management.html | grep -c "cpa-native-route-host"

# 导航路由是否存在
curl -s http://127.0.0.1:8317/management.html | grep -o 'path:`/cursor-api-key`'

# 浮动紫色按钮是否已彻底消失（计数应为 0）
curl -s http://127.0.0.1:8317/management.html | grep -c ":host{position:fixed;inset:0;z-index:2147482999"
```
