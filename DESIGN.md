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

### 三格式凭据导出

下载接口通过显式 `format` 参数区分 ChatGPT 网页会话（`web`）、Sub2API 与 CLIProxyAPI（CPA），缺省保持原有 Sub2API 格式以兼容旧客户端。网页会话导出包含 ChatGPT/OpenAI 域 Cookie、Cookie Header、注册 UA 和地区，单账号直接返回 JSON，多账号返回 ZIP；CPA 使用其 auth-dir 所需的扁平 `type=codex` JSON，并从 access token 的 JWT `exp` 生成 RFC3339 `expired`。文件名统一过滤路径分隔符，避免压缩包路径穿越。

注册流程只获得 ChatGPT 网页会话 access token，因此 CPA 导出的 `refresh_token` 和 `id_token` 为空。网页生图使用结构化 Cookie 恢复 ChatGPT 会话，不把 access token 直接当作 `api.openai.com/v1` 的 API 凭据。

### ChatGPT 网页会话持久化与测活

ChatGPT 登录态的边界是浏览器 Cookie，会话接口返回的 access token 只是短期派生凭据。注册结束、销毁临时 BrowserContext 前，系统保存 ChatGPT/OpenAI 域 Cookie 的 name/value/domain/path/expiry/httpOnly/secure/sameSite 元数据到 `auth_data.cookies`。开启预热时，真实网页对话必须完成，否则注册失败，不允许只拿到 token 的账号进入库存。

测活创建隔离浏览器上下文，恢复 Cookie、注册 UA、屏幕、时区、语言和原粘性代理 session，再由页面带 `credentials=include` 请求 `/api/auth/session`：HTTP 200 且返回 access token 才是 alive，明确 401 是 dead，Cloudflare 403、限流、网络错误为 unknown。旧版只有 access token 的记录无法重建原网页会话，一律 unknown，不再调用 `/backend-api/me` 制造假 401。

### 独立 Adobe 注册（Firefly 免费额度）

Adobe 注册与 ChatGPT、Grok 完全隔离：独立数据表 `adobe_registrations`、独立浏览器包 `internal/adobereg`、独立生产器 `internal/adobeproducer`、独立处理器 `handlers/adobe.go`、独立路由前缀 `/api/adobe/*` 与独立页面 `static/adobe.html`+`adobe.js`。不复用 Grok 的模型、路由或前端状态，也不共享浏览器二进制（使用独立 rod 目录 `browser-adobe`）。

注册目标是 Adobe 账号（免费即可用 Firefly 的免费生图/生视频额度）。浏览器流程：打开 `account.adobe.com` → 进入「创建账号」表单填邮箱+随机密码 → 第二步填姓名/生日/地区（默认 US）→ 提交创建 → 打开 Firefly 触发「验证身份」邮箱验证码页 → 自动取码填入。验证码复用现有邮箱池（`mailfetch` + `Mailbox` 凭据）：生产器按 `since` 时间轮询邮件，用发件人/主题/正文特征（`adobe`/`firefly`/`verification code` 等）筛出 Adobe 邮件并提取 6 位数字码；每条 Adobe 记录独立保存所用 `mailbox_id`。日志与列表接口都不输出验证码和 Cookie 明文。

注册成功后把 Adobe 登录会话（全量 Cookie + localStorage/sessionStorage）序列化进 `auth_data`（`json:"-"`，列表接口一律清空）。导出接口 `/api/adobe/download` 仅对已注册且有会话数据的记录开放，支持三种格式，导出即出库：

- `string`：浏览器 Cookie 头字符串 `k=v; k=v; ...`（单账号 `.txt`，多账号 `.zip`）。
- `json`：单个 Adobe 的 Cookie JSON 对象（含 `cookie_string`、`cookies_map`、带元数据的 `cookies` 数组、`storage`；单账号 `.json`，多账号 `.zip`）。
- `array`：多个 Adobe 批量导出的 Cookie 数组，始终单个 `.json` 文件。

生命周期与 Grok 一致：并发受全局 `max_concurrency`（默认 1）约束，代理走全局 `proxy_enabled` / `proxy_list`，无头由 `adobe_headless` 控制；服务启动时把残留的 `registering`/`waiting_code` 记录回收为 `register_failed`，可重新注册。Adobe 与 Grok 共用任务级代理会话策略：BestGo 动态住宅代理若未显式配置 `-session-`，生产器会为每个注册任务生成独立 session，不同任务允许轮换出口，但同一浏览器/协议流程（含 Turnstile 与 OAuth）保持出口 IP 稳定；认证代理日志同时记录本地桥入口和不含凭据的上游地址，避免把 `127.0.0.1` 误认为直连。

## 已知限制

- 面向单实例 SQLite 部署，不提供分布式任务锁。
- 全部删除是不可恢复操作，依赖数据库备份进行灾难恢复。
- 邮箱验证错误当前不作为结构化字段展示。
- IMAP 路径面向 Outlook OAuth，不支持密码登录或任意用户指定的 IMAP 主机；一次会话的网络操作受 20 秒截止时间约束。

## 安全边界与威胁模型

- **授权边界**：重新认证和全部删除接口全部位于现有 `/api` 鉴权组内，未登录请求不能触发后台任务或删除数据。
- **破坏性操作**：前端使用两次确认降低误触风险；账户生产任务运行时，后端以 `409 Conflict` 拒绝全部删除。
- **敏感信息**：后台调度日志只记录任务 ID 和数据库错误，不输出邮箱密码、refresh token 或 access token。
- **ChatGPT 会话秘密**：Cookie value 与 access token 都只存在数据库 `auth_data` 和受鉴权下载响应中；列表、日志、测活状态接口不返回明文。网页导出不附带代理密码或数据库中的账户密码。
- **IMAP 安全边界**：服务器地址固定为 `outlook.office365.com:993`，启用证书校验和 TLS 1.2 下限，避免用户输入形成 SSRF；XOAUTH2 响应和服务器认证挑战均不写日志。
- **页面渲染**：现有列表使用模板字符串生成静态结构，来自 API 的文本字段继续统一经过 `esc()` 转义。本次没有新增未经转义的动态 HTML。
- **已知风险**：管理员凭据被盗后仍可执行不可恢复的全部删除；应依赖强管理员密码、HTTPS 与数据库备份降低风险。

## 变更历史

### 2026-09-06 - ChatGPT 网页会话持久化、消除 token-only 401 误判

**变更内容**：注册成功后保存 ChatGPT/OpenAI 域完整 Cookie；开启预热时真实网页对话失败即注册失败；测活改为恢复 Cookie 后验证 `/api/auth/session`，旧 token-only 记录标 unknown；下载接口和管理页新增“网页会话”格式，单账号 JSON、多账号 ZIP。

**变更理由**：线上记录只保存 `/api/auth/session` 派生的 access token，随后销毁 BrowserContext；测活又在全新无 Cookie 的浏览器中用 Bearer 请求 `/backend-api/me`。该请求返回 401 只能说明凭据组合不被端点接受，不能证明原网页登录态失效，也无法满足网页生图所需的会话语义。

**影响范围**：`internal/codexreg` 会话采集与成功条件、`internal/livecheck` 与 `handlers/livecheck.go`、`handlers/produce.go`、GPT 账号管理页和文档。旧数据无 Cookie，不能无损补回，只能保持 unknown 或重新登录/注册。

**决策依据**：验证目标是 ChatGPT 网页会话，因此持久化和测活都以 Cookie 会话为边界；access token 导出仅保留兼容，不再作为网页会话存活判据。

### 2026-09-03 - ChatGPT 注册指纹与行为对齐真人、测活去关联

**背景**：注册出来的号在生第一张图前后大量被停用，且常常整批一起死，说明问题主要在"账号被关联"和"注册即被打上自动化标记"，而不是单个号的使用量。对比新旧浏览器指纹后确认旧注册浏览器有多处硬伤：`Network.setUserAgentOverride` 写死 Chrome/150 却没带 `userAgentMetadata`，导致 Client Hints 整体消失（`Sec-CH-UA` 不发、`navigator.userAgentData.brands` 为空）；`AcceptLanguage` 传了带 q 值的字符串，发出去的请求头是畸形的 `en-US,en;q=0.9;q=0.9`；rod 默认套 "Mac Chrome/114 1280x800" 设备模拟，`devicePixelRatio` 变成 1.0000000149；stealth 把 WebGL 伪造成 Mac 的 "Intel Iris OpenGL Engine"、`hardwareConcurrency` 写死 4，与 Win32 UA 互相矛盾；实际运行的是 Chrome 128 旧 headless；所有点击是 `element.click()`（`isTrusted=false`），所有输入是一次性 `insertText`（零按键事件）。测活则用服务器同一 IP、同一个浏览器并发探测所有账号的 token，直接把整批号关联在一起。

**变更内容**：
- 新增 `codexreg/launch.go`：`LaunchBrowser` / `Session.NewPage`。new headless；删掉 rod 的 Puppeteer 风格默认参数、随机非零调试端口、`NoDefaultDevice`；优先本机安装的 Chrome/Edge（设置 `chatgpt_browser_bin`，`rod` 强制内置 Chromium）；有认证的 http 代理走本地认证桥；UA 取浏览器真实 UA 只去掉 Headless 标记，Client Hints 从一个回环安全上下文页读出真实值、仅把 `HeadlessChrome` 品牌换回 `Google Chrome`/丢弃（Chromium）后原样回写；语言列表不带 q 值；每次随机一套常见分辨率并用 `setDeviceMetricsOverride` 还原"屏幕 > 窗口 > 视口"层次。
- 新增 `codexreg/human.go`：`HumanClick`（光标缓动轨迹 + 按下/抬起，`isTrusted=true`）、`HumanType`（逐键 keydown/keyup，Shift 字符带修饰键，随机间隔）；`browser.go` 全部改用它们并加入步骤间随机停顿，去掉 stealth。
- 新增 `codexreg/warmup.go`：注册成功后在同一浏览器发一条普通问题并等回复（设置 `chatgpt_warmup`，默认开），失败只记日志不影响结果。
- `Result`/`auth_data` 新增 `user_agent`、`screen`、`registered_ip`、`registered_country`、`registered_timezone`、`registered_at`、`warmed_up`、`proxy`，供下游沿用同 UA / 同地区用号；导出格式（Sub2API / CPA）不变。
- `livecheck.CheckChatGPT` 改为每个账号独立起一个与注册同指纹的浏览器、独立代理出口（优先沿用 `auth_data.proxy` 的线路，BestGo 每号换 session），逐个探测并错开间隔；`handlers` 按账号数放大超时。删除 `livecheck/browser.go`。
- 姓名池从 15×15 扩到 60×60；设置页新增「GPT 注册后预热对话」「GPT 注册浏览器」。

**影响范围**：`internal/codexreg`、`internal/livecheck`、`internal/handlers/livecheck.go`、`internal/producer/producer.go`、`static/settings.html`、文档。单号注册耗时增加约 30~90 秒（真人节奏 + 预热）；全量测活改为串行、每号约 6~10 秒。Grok / Adobe / Leonardo 等其它平台不受影响。仍无法在本项目内解决的风险：`+别名` 裂变子号与母号的天然关联，以及下游网关用固定服务器 IP、无浏览器指纹批量调用 backend-api 的使用方式。

### 2026-09-03 - ChatGPT 共享浏览器进程池、IP 拦截识别、线上实测

**背景**：用户要求"全协议 + 存活率"。评估结论：ChatGPT 注册链路每步都要 OpenAI Sentinel（VM 混淆 JS 指纹载荷 + PoW + Turnstile/Arkose），脚本几周换版，外层 Cloudflare 还校验 TLS/HTTP2 ↔ UA ↔ Client Hints ↔ Sentinel 自洽性；纯协议既是常态化跟版，其产出账号的画像（tls-client 指纹 + 零前端遥测）又正是批量封号的画像，与"存活率"目标相悖（Grok 能纯协议是因为 x.ai 只有一层 Turnstile）。用户真实诉求是资源与速度，改为在真实 Chrome 路线上做"共享进程池"。

同时巡检线上库（457 已注册 / 176 失败）：9-02 晚并发 7、`proxy_enabled=0` 直连机房 IP（Arosscloud AS400619，`hosting=true`）时 168 个失败全是 `context deadline exceeded`，截图两类：Cloudflare「Verify you are human」整页拦截、提交邮箱后按钮永远转圈——都是出口 IP 被拦，旧代码却按"邮箱失败"白等 60 秒再进 30 分钟冷却。

**变更内容**：
- `codexreg/launch.go` 拆出 `host`（Chrome 进程：真实 UA / Client Hints，进程级属性）；新增 `codexreg/pool.go`：`Pool.Acquire` 用 `Target.createBrowserContext{proxyServer}` 为每个账号建独立上下文（cookie / 缓存 / 代理出口 / 窗口尺寸 `Browser.setWindowBounds` / 屏幕 / 语言 / 时区互不共享），`ContextsPerHost` 控制每进程账号数，进程按 `HostMaxAge` / `HostMaxContexts` 退役、最后一个上下文释放后关闭。有认证的 http 代理在池模式下同样走本地认证桥（进程级 `HandleAuth` 会串号，不支持带账号密码的 socks5）。`Session.Close` 区分独占进程 / 归还上下文。本地 `fptest -mode pool` 验证：同进程两个上下文屏幕不同、cookie 不串、第二个上下文分配 2ms。加 `--force-webrtc-ip-handling-policy=default_public_interface_only` 防 STUN 泄露真实 IP。
- `codexreg/browser.go`：`Race()+MustDo()` 状态机改为不 panic 的轮询（`pollState` / `pollStateJS`），加 `waitEmailForm`；识别 Cloudflare 整页 / 可见 Turnstile 与"提交后按钮持续处理态无响应"为新错误 `ErrIPBlocked`；按钮未进入处理态则先重提交一次再判定。页面句柄改为 `base`（只绑任务 ctx）+ 每步派生 `Timeout`，不再链式 `CancelTimeout()`——它会 cancel 掉上一步的 ctx，`HumanType` 等保留旧句柄的调用就会 `context canceled`。提交按钮定位改为与语言无关的 `submitButtonJS`（表单内 submit → 唯一主按钮 → 回车），日文界面下不再"未找到提交按钮"。`human.go` 不再用 rod 的 `ScrollIntoView/Focus/SelectAllText`（内含 `WaitStableRAF`，遇到持续动画会等到超时，实测资料页填两个框耗尽 60 秒），改为直接 DOM 调用。
- `codexreg/warmup.go`：实测新账号第一次发消息会弹「準備が完了しました / You're all set」条款确认层，可能在进入主界面时、也可能在第一次敲键时出现并吞掉发送；改为发送前后都检测（`gateButton`：屏幕中央顶层大按钮，排除顶部 Chat/Work 分段控件）并点掉、被吞则重发；输入框识别用 `findPromptBox`（多套候选选择器 + `elementFromPoint` 确认顶层可点到）；回复判定同时看 assistant 节点 / 停止按钮 / URL 进入 `/c/` / 用户消息。预热失败保存截图。
- `producer`：按 `chatgpt_browser_pool`（默认开）/ `chatgpt_contexts_per_host`（默认 4）建池、任务结束关池；`ErrIPBlocked` 与 `ErrTermsRejected` 一样换住宅 IP 重试（最多 3 次），耗尽后标 `register_failed` 并提示开代理。GeoIP 查询超时 30s → 12s。`livecheck` 批量时也用池（每号一个上下文）。设置页新增两项。
- 姓名池等前一条目内容不变。

**验证**：服务器（16C/16G，Chrome 151，BestGo JP 住宅代理）连续 6 次单号端到端注册成功，耗时 82~185 秒（含 20~75 秒预热）；池模式并发 2 号同一进程、各自出口 IP 不同，1m43s / 1m58s 双成功、`warmed=true`，进程用完即关（`hosts=1 active=0`）。第一次 e2e 直连模式复现线上"提交无响应"，现已识别为 `ErrIPBlocked`。

**影响范围**：`internal/codexreg/{launch,pool,browser,human,warmup}.go`、`internal/producer/producer.go`、`internal/livecheck/chatgpt.go`、`internal/handlers/livecheck.go`、`static/settings.html`、文档。线上建议：开启 `proxy_enabled`（直连机房 IP 会被 Cloudflare 拦）、并发 6~8、每进程 4 个上下文；`fission_count` 视存活率需要调低。

### 2026-08-10 - Adobe 注册不再使用裂变账号

**变更内容**：`adobeproducer.StartFromAccounts` 从 ChatGPT 账号池取号时加 `is_mother = 1` 过滤，只用母号；裂变号是 `+别名` 邮箱，Adobe 把它视作同一邮箱，用来注册会撞号。不足部分仍从已验证邮箱池补齐，逻辑不变。

### 2026-08-10 - Adobe 注册提速与并发/代理配置归一

**变更内容**：Adobe 建号后先用浏览器 Cookie 直接探一次 cookie→token，命中身份核验就直奔核验页（核验取码与页面加载并行），不再先打开 Firefly 白等就绪超时；探测失败才回退原来的 `passAdobeRide` 路径，Firefly 就绪等待从 25 秒收到 15 秒，各处页面轮询从 700ms~1s 收到 250~300ms，收码轮询 5 秒收到 3 秒。配置上删掉 `adobe_max_concurrency` / `grok_max_concurrency`，两个生产器只读全局 `max_concurrency`；Adobe 代理不再读 `adobe_proxy_enabled` / `adobe_proxy_list`，统一跟全局 `proxy_enabled` / `proxy_list`。无头仍按平台分开（`adobe_headless` / `grok_headless`）；Adobe 无头照搬 grokreg 的方案——显式 `--headless=new` 并把 UA 里的 HeadlessChrome 标记改回普通 Chrome（否则注册页停在加载动画进不了表单）。后续再收紧：收码轮询 3 秒收到 2 秒，模拟人手输入间隔 40~110ms 收到 25~70ms；验证码提交失败重试前先点「重新发送」，否则重试会一直等一封不会再来的新邮件直到收码超时。实测无头并发 2 单号约 52~58 秒；并发提到 4 时单号退化到 71~78 秒且一半卡在验证码重试，故全局并发保持 2。同 IP 连注约 30 个号后 Adobe 会弹 hCaptcha 图形验证（提交步永远推进不了）：`submitAndAdvance` 超时前检测 `iframe[src*="hcaptcha.com"]`，命中直接报 `errCaptchaPuzzle` 快速失败并提示开代理，第一步也不再做无意义的整页重试；规避方式是开启全局代理（BestGo 动态住宅代理按任务轮换出口 IP）。

**动机**：线上日志显示单号约 83 秒里有 32 秒是「先开 Firefly、等就绪超时、才发现被核验拦住」的纯白等；同时按平台各一把并发/代理键容易和设置页上的全局值不一致，出现 Adobe 排队逐个跑的情况。

### 2026-08-08 - Turnstile 签发脚本纳入仓库

**变更内容**：把 `turnstile_mint.py` / `turnstile_pool.py` 收进 `scripts/`（附 `scripts/requirements.txt` 记录 venv 依赖版本），`turnstileScriptPath` 只在我们自己的位置查找，顺序为「可执行文件同目录 scripts → `<prefix>/share/chatgpt-register/scripts` → 当前目录 scripts → `/usr/local/share/chatgpt-register/scripts`」，都找不到直接报错并列出查找过的目录，不再回落到其他项目装在 `/usr/local/share/grok-reg` 的副本；venv 缺失时 python 回落到系统 `python3`；日志新增一行打印实际使用的脚本与解释器。

**变更理由**：这两个脚本原本是另一个项目（/opt/Grok-Register）的 install.sh 装到 `/usr/local/share/grok-reg/` 的，同机的 grok-web.service 也在用同一份——仓库里没有，本地开发跑不了签令牌，对方卸载/升级还会连带把我们的注册搞挂。

**影响范围**：`internal/grokreg/turnstile_mint.go`、`scripts/`。部署需把 `scripts/*.py` 装到 `/usr/local/share/chatgpt-register/scripts/`（服务器已装）；实测服务二进制解析到部署副本、仓库内跑解析到 `scripts/` 副本，直连注册一个号 31 秒、Console 额度正常。CloakBrowser 的 venv（`/opt/cloakbrowser-venv`）仍是外部依赖，按 requirements.txt 可重建。

### 2026-08-08 - Grok 代理跟随全局开关

**变更内容**：删除 `grok_proxy_enabled` / `grok_proxy_list` 这两个隐藏键的读取逻辑，Grok 注册直接跟设置页上的 `proxy_enabled` + `proxy_list`。

**变更理由**：设置页只有全局开关，Grok 私有开关在库里为 1 时，页面显示"未启用代理"但实际在走代理，看不出来也没法关。

**影响范围**：`internal/grokproducer/producer.go`。服务器上这两个键已删。

### 2026-08-08 - Grok 注册配置缓存

**变更内容**：`sitekey / Next-Action id / router state tree` 在进程内缓存 20 分钟（`signupConfigTTL`），命中缓存时只做一次 `WarmSignup` 养 cookie + 探 Cloudflare，跳过抓 `_next` JS chunk 找 action id；注册成功后写入缓存，注册未拿到 SSO 或缓存 warm 不过则丢控重抓。Turnstile 令牌签发也提前到取码之前启动，与取码、等码全程重叠。

**变更理由**：这三个值只跟着 x.ai 发版变，每个号重抓要多花 3～22 秒。

**影响范围**：`internal/grokreg/protocol_register.go`。实测同进程连续注册：首号 37 秒（抓配置 10 秒），次号 26 秒（复用配置 1 秒）。此时耗时几乎全在 Turnstile 签发（经住宅代理约 23 秒，直连 13 秒），且已完全与收码重叠。

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

**变更内容**：GPT 注册增加 Sub2API 与 CPA 两个直接导出选项；后端生成 CPA 扁平认证 JSON，批量时按账号打包 ZIP，并从 JWT 提取准确过期时间。

**变更理由**：让已注册账号无需手工转换即可放入 CLIProxyAPI 的 `auth-dir`，同时保持原有 Sub2API 导出接口兼容。

**影响范围**：账户下载接口、CPA 文件打包与命名、GPT 注册交互、单元测试及使用文档。

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

**影响范围**：邮箱导入与状态流转、Microsoft OAuth 重试、Gin 路由、邮箱管理、GPT 注册、仪表盘、项目与模块文档。
