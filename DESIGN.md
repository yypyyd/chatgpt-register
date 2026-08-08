# 设计说明

## 系统边界

本项目是单实例 Go 管理服务：Gin 提供受 Bearer token 保护的 API 和静态管理页面，GORM/SQLite 保存邮箱、注册账户与设置，后台生产器和邮箱认证调度器执行长任务。

## 关键设计决策

### 持久化邮箱认证

邮箱批量认证由 `internal/mailverify` 在服务端执行，数据库状态承担持久队列职责。浏览器只提交命令和轮询列表，不再持有任务生命周期。服务启动时恢复遗留的 `verifying` 记录。

### 批量删除

邮箱和账户分别提供集合级 `DELETE` 接口。前端执行双重确认；账户生产任务运行时后端拒绝全部删除，避免破坏正在执行的任务。

### OAuth 重试

`invalid_grant` 等确定性 refresh token 错误立即失败；网络错误、HTTP 429/5xx 和 OAuth 临时错误最多重试三次。Outlook 在并发时偶尔会把临时节流表现为普通 IMAP `AUTHENTICATE failed`，因此令牌刷新成功后的 IMAP 拒绝也会重新换取令牌后短暂重试。这样既保留抗瞬时故障能力，也避免无效 refresh token 额外等待固定退避。

### Graph 与 IMAP 双协议取件

`internal/mailfetch` 首先请求 Microsoft Graph `.default`。Graph 刷新成功时使用 Graph 邮件文件夹接口；遇到旧式 Outlook 刷新令牌特有的 `AADSTS90023` 时，改为不传 `scope`，再根据授权范围选择 Outlook IMAP XOAUTH2。认证不是只检查令牌端点：Graph 会读取收件箱文件夹，IMAP 会完成 XOAUTH2 登录和 `NOOP`。

IMAP 读取固定使用 TLS 993 和最低 TLS 1.2，读取 `INBOX`，并通过 `LIST` 的 `\Junk` 属性发现垃圾邮件目录（兼容 `Junk` / `Junk Email`）。列表只取信封头，正文按需通过 UID FETCH 获取；对外 ID 使用 `imap:<base64-folder>:<uid>` 的不透明格式，MIME 正文限制为单个文本部分最多 8 MiB。访问令牌连同协议类型缓存在内存中，不写入数据库或日志。

### 双格式凭据导出

下载接口通过显式 `format` 参数区分 Sub2API 与 CLIProxyAPI（CPA），缺省保持原有 Sub2API 格式以兼容旧客户端。CPA 使用其 auth-dir 所需的扁平 `type=codex` JSON，并从 access token 的 JWT `exp` 生成 RFC3339 `expired`；单账号直接返回 JSON，多账号返回 ZIP 且每个账号独立成文件。文件名会过滤路径分隔符，避免压缩包路径穿越。

注册流程只获得 ChatGPT 网页会话 access token，因此 CPA 导出的 `refresh_token` 和 `id_token` 为空。导出格式转换不改变上游授权范围，也不能解决账号本身对 Codex Responses 的 401；凭据过期后不能由 CPA 自动刷新。

### 独立 Adobe 注册（Firefly 免费额度）

Adobe 注册与 ChatGPT、Grok 完全隔离：独立数据表 `adobe_registrations`、独立浏览器包 `internal/adobereg`、独立生产器 `internal/adobeproducer`、独立处理器 `handlers/adobe.go`、独立路由前缀 `/api/adobe/*` 与独立页面 `static/adobe.html`+`adobe.js`。不复用 Grok 的模型、路由或前端状态，也不共享浏览器二进制（使用独立 rod 目录 `browser-adobe`）。

注册目标是 Adobe 账号（免费即可用 Firefly 的免费生图/生视频额度）。浏览器流程：打开 `account.adobe.com` → 进入「创建账号」表单填邮箱+随机密码 → 第二步填姓名/生日/地区（默认 US）→ 提交创建 → 打开 Firefly 触发「验证身份」邮箱验证码页 → 自动取码填入。验证码复用现有邮箱池（`mailfetch` + `Mailbox` 凭据）：生产器按 `since` 时间轮询邮件，用发件人/主题/正文特征（`adobe`/`firefly`/`verification code` 等）筛出 Adobe 邮件并提取 6 位数字码；每条 Adobe 记录独立保存所用 `mailbox_id`。日志与列表接口都不输出验证码和 Cookie 明文。

注册成功后把 Adobe 登录会话（全量 Cookie + localStorage/sessionStorage）序列化进 `auth_data`（`json:"-"`，列表接口一律清空）。导出接口 `/api/adobe/download` 仅对已注册且有会话数据的记录开放，支持三种格式，导出即出库：

- `string`：浏览器 Cookie 头字符串 `k=v; k=v; ...`（单账号 `.txt`，多账号 `.zip`）。
- `json`：单个 Adobe 的 Cookie JSON 对象（含 `cookie_string`、`cookies_map`、带元数据的 `cookies` 数组、`storage`；单账号 `.json`，多账号 `.zip`）。
- `array`：多个 Adobe 批量导出的 Cookie 数组，始终单个 `.json` 文件。

生命周期与 Grok 一致：并发受 `adobe_max_concurrency`（回退 `max_concurrency`，默认 1）约束，代理走 `adobe_proxy_*`（回退全局 `proxy_*`），无头由 `adobe_headless` 控制；服务启动时把残留的 `registering`/`waiting_code` 记录回收为 `register_failed`，可重新注册。Adobe 与 Grok 共用任务级代理会话策略：BestGo 动态住宅代理若未显式配置 `-session-`，生产器会为每个注册任务生成独立 session，不同任务允许轮换出口，但同一浏览器/协议流程（含 Turnstile 与 OAuth）保持出口 IP 稳定；认证代理日志同时记录本地桥入口和不含凭据的上游地址，避免把 `127.0.0.1` 误认为直连。

## 已知限制

- 面向单实例 SQLite 部署，不提供分布式任务锁。
- 全部删除是不可恢复操作，依赖数据库备份进行灾难恢复。
- 邮箱验证错误当前不作为结构化字段展示。
- IMAP 路径面向 Outlook OAuth，不支持密码登录或任意用户指定的 IMAP 主机；一次会话的网络操作受 20 秒截止时间约束。

## 安全边界与威胁模型

- **授权边界**：重新认证和全部删除接口全部位于现有 `/api` 鉴权组内，未登录请求不能触发后台任务或删除数据。
- **破坏性操作**：前端使用两次确认降低误触风险；账户生产任务运行时，后端以 `409 Conflict` 拒绝全部删除。
- **敏感信息**：后台调度日志只记录任务 ID 和数据库错误，不输出邮箱密码、refresh token 或 access token。
- **IMAP 安全边界**：服务器地址固定为 `outlook.office365.com:993`，启用证书校验和 TLS 1.2 下限，避免用户输入形成 SSRF；XOAUTH2 响应和服务器认证挑战均不写日志。
- **页面渲染**：现有列表使用模板字符串生成静态结构，来自 API 的文本字段继续统一经过 `esc()` 转义。本次没有新增未经转义的动态 HTML。
- **已知风险**：管理员凭据被盗后仍可执行不可恢复的全部删除；应依赖强管理员密码、HTTPS 与数据库备份降低风险。

## 变更历史

### 2026-08-08 - Turnstile 签发脚本纳入仓库

**变更内容**：把 `turnstile_mint.py` / `turnstile_pool.py` 收进 `scripts/`（附 `scripts/requirements.txt` 记录 venv 依赖版本），`turnstileScriptPath` 只在我们自己的位置查找，顺序为「可执行文件同目录 scripts → `<prefix>/share/chatgpt-register/scripts` → 当前目录 scripts → `/usr/local/share/chatgpt-register/scripts`」，都找不到直接报错并列出查找过的目录，不再回落到其他项目装在 `/usr/local/share/grok-reg` 的副本；venv 缺失时 python 回落到系统 `python3`；日志新增一行打印实际使用的脚本与解释器。

**变更理由**：这两个脚本原本是另一个项目（/opt/Grok-Register）的 install.sh 装到 `/usr/local/share/grok-reg/` 的，同机的 grok-web.service 也在用同一份——仓库里没有，本地开发跑不了签令牌，对方卸载/升级还会连带把我们的注册搞挂。

**影响范围**：`internal/grokreg/turnstile_mint.go`、`scripts/`。部署需把 `scripts/*.py` 装到 `/usr/local/share/chatgpt-register/scripts/`（服务器已装）；实测服务二进制解析到部署副本、仓库内跑解析到 `scripts/` 副本，直连注册一个号 31 秒、Console 额度正常。CloakBrowser 的 venv（`/opt/cloakbrowser-venv`）仍是外部依赖，按 requirements.txt 可重建。

### 2026-08-08 - Grok 代理跟随全局开关

**变更内容**：删除 `grok_proxy_enabled` / `grok_proxy_list` 这两个隐藏键的读取逻辑，Grok 注册直接跟设置页上的 `proxy_enabled` + `proxy_list`。

**变更理由**：设置页只有全局开关，Grok 私有开关在库里为 1 时，页面显示"未启用代理"但实际在走代理，看不出来也没法关。

**影响范围**：`internal/grokproducer/producer.go`。服务器上这两个键已删。

### 2026-08-08 - Grok 注册配置缓存

**变更内容**：`sitekey / Next-Action id / router state tree` 在进程内缓存 20 分钟（`signupConfigTTL`），命中缓存时只做一次 `WarmSignup` 养 cookie + 探 Cloudflare，跳过抓 `_next` JS chunk 找 action id；注册成功后写入缓存，注册未拿到 SSO 或缓存 warm 不过则丢控重抓。Turnstile 令牌签发也提前到取码之前启动，与取码、等码全程重叠。`cmd/groktest` 支持一次传多个配置在同进程内顺序注册，便于验证跨账号复用。

**变更理由**：这三个值只跟着 x.ai 发版变，每个号重抓要多花 3～22 秒。

**影响范围**：`internal/grokreg/protocol_register.go`、`cmd/groktest/main.go`。实测同进程连续注册：首号 37 秒（抓配置 10 秒），次号 26 秒（复用配置 1 秒）。此时耗时几乎全在 Turnstile 签发（经住宅代理约 23 秒，直连 13 秒），且已完全与收码重叠。

### 2026-08-08 - Grok 协议注册提速

**变更内容**：Turnstile 令牌改为在等验证码期间并行签发（令牌有效期约 5 分钟，与收码重叠后基本不占关键路径）；删除注册后的 `CreateSession` 换新 SSO 步骤（`createFreshSessionSSOProto`），直接用 Server Action 返回的注册 SSO。

**变更理由**：换新 SSO 那步原本是为 CPA 设备授权准备的（设备授权对全新 SSO 更挑），CPA 已移除后不再需要；它自身还要再签一次 Turnstile，实测占用约 28 秒。实跑验证：注册 SSO 直接过 Console DPoP + `/v1/usage`，额度 chat 10/10 · image 5/5 · video 2/2。

**影响范围**：`internal/grokreg/protocol_register.go`。单号实测 77 秒（旧 browser 引擎）→ 47 秒；并发 2 时 2 个号 56 秒完成（约 28 秒/号），瓶颈是 CloakBrowser 签发令牌的 CPU 占用。

### 2026-08-08 - Grok 无头注册可用

**变更内容**：`grok_headless=1` 时改用 new headless（`--headless=new`），Turnstile 补丁扩展在无头下同样加载（`window.__cfSolve` 是注入签发 token 的必要条件），并把无头 UA 里的 `HeadlessChrome` 标记改回普通 Chrome（`Emulation.setUserAgentOverride`）。

**变更理由**：旧 `--headless` 在 Chrome 128 下忽略扩展，无头会拿不到 `__cfSolve`，且 UA 暴露 HeadlessChrome 会被 Cloudflare 直接拦。

**影响范围**：`internal/grokreg/browser.go`。默认引擎 `protocol` 本身就不开可见浏览器（浏览器只用于签 Turnstile，且 CloakBrowser 走 offscreen），实测一个号约 57 秒；`grok_engine=browser` 的旧全程浏览器流程约 77 秒。

### 2026-08-08 - Grok 导出去掉 CPA，只留 Console 与 Sub2API

**变更内容**：Grok 下载接口的 `format` 只接受 `console`（默认）与 `sub2api`，删除 CPA(xAI OAuth) 导出分支与打包逻辑；页面按钮改为「导出 Console」/「导出 Sub2API」，行内按钮为 Console 与 Sub。

**变更理由**：CPA 号池不再使用，Grok 只出 Console 号和 Sub2API 的 sso 池。

注册流程里为 CPA 加的那步（OAuth 设备码流程铸造 `cpa_xai`）也一并移除：删掉 `internal/grokreg/cpa_mint.go` 和 `internal/grokreg/oauth/`，协议注册与浏览器注册都不再跑设备授权，注册少一次外部往返和一处失败点。新号 `AuthData` 不再有 `cpa_xai`，测活只走 Console 探测（老号仍可用 refresh_token 回退）。

**影响范围**：`internal/handlers/grok.go`（删除 `grokCPAAuth`/`downloadGrokCPA`，恢复 `downloadGrokSub2API`）、`internal/grokreg/browser.go`、`internal/grokreg/protocol_register.go`、删除 `internal/grokreg/cpa_mint.go` 与 `internal/grokreg/oauth/oauth.go`、`static/grok.html`、`static/grok.js`。

### 2026-08-08 - Grok 测活改为 Console 额度探测

**变更内容**：Grok 测活优先用账号 sso token 走 console.x.ai 的 DPoP 流程（`POST /v1/dpop/token` 换一次性 token，再带 DPoP proof 读 `GET /v1/usage`），把 chat/image/video 额度写入新字段 `console_quota` 并在列表「存活」列展示；没有 sso 的旧账号仍回退到 xAI OAuth refresh_token 授权探测。401 或非 Cloudflare 的 403 判死，Cloudflare / 429 / 5xx / 超时保持 unknown。

**变更理由**：导出目标已经是 Console 号，测活也应该验证 Console 这条链路，并顺带给出账号真实可用额度，而不是只验证 OAuth 凭据。

**影响范围**：新增 `internal/livecheck/grokconsole.go`；`internal/livecheck/grok.go`（`GrokItem.SSO` + `GrokResult`）、`internal/handlers/livecheck.go`、`internal/models/grok.go`（新增 `console_quota` 列，AutoMigrate 自动补）、`static/grok.js`、`static/style.css`；ChatGPT/Adobe 测活不变。

### 2026-08-08 - Grok 导出改用 grok2api Console 格式

**变更内容**：Grok 页面的 sso 导出由旧的 `{"ssoBasic":[…]}` 池改为 grok2api 的 Grok Console 账号导入 JSON（`{"provider":"console","accounts":[{"name","email","sso_token"}]}`），`format` 取值改为 `console`；旧值 `sub2api` 与缺省仍按 `console` 处理。

**变更理由**：grok2api 现在按 Provider 导入账号，Console 号池只认 `provider=console` + `sso_token` 的文档格式，旧 `ssoBasic` 池无法直接导入。

**影响范围**：`internal/handlers/grok.go` 的 Grok 下载接口与文件名，`static/grok.html`、`static/grok.js` 的导出入口文案；不改动 CPA 导出和 ChatGPT/Adobe 导出。

### 2026-07-28 - 新增独立 Adobe（Firefly）注册

**变更内容**：新增与 ChatGPT/Grok 隔离的 Adobe 注册子系统：`models.AdobeRegistration` + `adobe_registrations` 表、`internal/adobereg` 浏览器流程、`internal/adobeproducer` 生产器（复用邮箱池自动取码）、`handlers/adobe.go`、`/api/adobe/*` 路由、`static/adobe.html`+`adobe.js` 页面与侧边栏入口；Cookie 导出支持字符串 / JSON 对象 / 批量数组三种格式。

**变更理由**：用户需要在同一管理台注册可用 Firefly 免费生图/生视频的 Adobe 账号，且不能与现有平台的表、路由、页面状态混用。

**影响范围**：新增独立包与页面；`main.go`（启动回收 + 路由 + 静态页）、`internal/db/db.go`（AutoMigrate + 孤儿回收）、`internal/handlers/registration.go`（Handler 注入 `AdobeProducer`）、`static/layout.js`（侧边栏）少量接线；不改动 ChatGPT/Grok 既有行为。

### 2026-07-28 - 裂变邮箱改用 plus addressing

**变更内容**：裂变子号由 `email-001@…` 改为 `email+001@…`，母号解析和邮箱注册数量统计同步按 `+` 别名识别。

**变更理由**：`-` 是邮箱本地部分的普通字符，不会投递到母号；支持 plus addressing 的邮箱服务会将 `+tag` 地址投递到母号。

**影响范围**：裂变邮箱生成、母号归属恢复、邮箱注册数量统计、单元测试及使用文档。

### 2026-07-25 - 增加 CPA 凭据导出

**变更内容**：账户管理增加 Sub2API 与 CPA 两个直接导出选项；后端生成 CPA 扁平认证 JSON，批量时按账号打包 ZIP，并从 JWT 提取准确过期时间。

**变更理由**：让已注册账号无需手工转换即可放入 CLIProxyAPI 的 `auth-dir`，同时保持原有 Sub2API 导出接口兼容。

**影响范围**：账户下载接口、CPA 文件打包与命名、账户管理交互、单元测试及使用文档。

**决策依据**：对照 CLIProxyAPI 当前 `CodexTokenStorage` 与文件加载器字段定义；空缺的 OAuth refresh/id token 保持为空，不伪造不可用凭据。

### 2026-07-25 - 重新认证范围选择

**变更内容**：邮箱管理页的“重新认证”提供“仅认证失败邮箱”和“认证全部邮箱”两个明确选项；存在勾选项时只显示并执行“认证所选邮箱”。后端为失败状态提供独立入队方法，并拒绝混合或空范围请求。

**变更理由**：避免重试单个失败邮箱时误将所有已认证邮箱重新入队，同时保留显式的全量认证入口。

**影响范围**：邮箱管理交互、重新认证接口请求体、后台邮箱认证服务及测试。

### 2026-07-25 - Outlook Graph / IMAP 双协议兼容

**变更内容**：令牌刷新支持 `AADSTS90023` 自动回退，按 OAuth scope 选择 Graph 或 IMAP；新增 IMAP XOAUTH2 真实认证、收件箱/垃圾箱列表、UID 正文读取与 MIME 解析。

**变更理由**：现有 91 个邮箱的刷新令牌只授权 `IMAP.AccessAsUser.All` 等 Outlook 协议权限，不能访问 Graph；仅使用 Graph `.default` 会把全部有效凭据误判为失败。

**影响范围**：`internal/mailfetch` 协议选择、邮箱认证、网页取件、注册验证码读取、Go 依赖与项目文档。

**决策依据**：保留 Graph 路径兼容现有令牌，同时以固定 Outlook TLS 端点承载旧式 IMAP OAuth 令牌；两条路径都验证真实邮箱访问能力。

### 2026-07-25 - 后台邮箱认证与列表批量删除

**变更内容**：把邮箱认证从浏览器并发任务迁移到持久化服务端 worker；增加重新认证、邮箱全部删除、账户全部删除，并在所有账户列表页面提供入口。

**变更理由**：页面刷新会导致原批量任务永久中断；逐条删除大量记录效率低且容易遗漏。

**影响范围**：邮箱导入与状态流转、Microsoft OAuth 重试、Gin 路由、邮箱管理、账户管理、仪表盘、项目与模块文档。
