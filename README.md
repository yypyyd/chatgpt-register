# chatgpt-register

> **ChatGPT 账号自动注册管理台** · 协议主流程 + 官方 Sentinel SDK · 凭据真实验证后出库

---

🌐 **生图站** [vividai.run](https://vividai.run) &nbsp;|&nbsp;
👥 **QQ 交流群** [1106849765](https://qm.qq.com/q/1106849765) &nbsp;|&nbsp;
🐧 **QQ** 1114639355 &nbsp;|&nbsp;
🛒 **小店** [pay.ldxp.cn/shop/chiyi](https://pay.ldxp.cn/shop/chiyi) &nbsp;|&nbsp;
✉️ **邮箱** [vividairun@gmail.com](mailto:vividairun@gmail.com)

---

## ✨ 核心优势

| 🔐 真实授权验证 | ✅ 全自动零干预 | 🔁 母号裂变子号 |
|:---:|:---:|:---:|
| 注册关键动作使用 OpenAI 官方 Sentinel SDK，注册和导出均以真实 backend-api 探针为准 | 验证码自动从邮箱读取，全流程零手动操作 | 每个邮箱注册 1 个母号 + N 个别名子号；别名天然有关联风险，默认应保守使用 |

| 🌐 代理池轮转 | 📊 可视化管理台 | 📦 Go 服务部署 |
|:---:|:---:|:---:|
| 内置代理池按账号轮转，多 IP 并发注册不封号 | 毛玻璃风格 UI，实时仪表盘 + 执行日志可视化 | Go 二进制内置管理界面，按渠道准备 Node.js 或 Chromium 运行环境 |

---

## 🤖 纯协议注册——技术亮点

> 注册以 Chrome 146 PSK TLS/HTTP2 客户端完成 HTTP 主流程；官方 Sentinel SDK 在 Node 假浏览器里执行，fetch 走同一条协议 TLS，不开 Chrome。失败即中止，不会本地伪造。假浏览器会在页面加载时启动官方的 Session Observer 收集器，并按真人节奏合成鼠标/键盘事件，环境字段（navigator / localStorage / 文本盒模型 / 屏幕 DPR）与真实 Chrome 逐项对齐——否则账号会在注册后 6~16 分钟被异步撤销 access token（`token_revoked`）。出口 IP 归属地决定语言/时区，完整 Cookie 会话与 access token 会保存。导出直接使用库存凭据，不在下载时走代理；测活仍单独检查。

### 注册全流程（全自动，无需人工）

```
查出口 IP 归属地 → 按地区决定语言 / 时区
    ↓
Chrome TLS 指纹直连 chatgpt.com → 首页 → /auth/login_with → providers / csrf → signin → authorize（自动发出邮箱验证码）
验证码页 / 资料页加载时，Node 假浏览器调用官方 Sentinel SDK 的 init(flow)，Session Observer 开始观察，
等待期间做稀疏的鼠标漂移（Windows Chrome 146 环境，网络走协议 TLS）
    ↓
实时监听邮箱，自动读取 6 位验证码；在页面上停留 14~32 秒后，按真人节奏点输入框、逐位输入、点提交并签发 token
（每轮最长等待 1 分钟，未收到新验证邮件时协议引擎自动重发一次）
    ↓
补全资料（姓名/生日）→ 回调换取会话 → 取 accessToken
    ↓
保存登录 Cookie 与 accessToken，并记录注册 UA / 出口 IP / 地区 / 代理（不注册 Agent Identity）
    ↓
导出 ChatGPT 账号凭据 JSON
    ↓
写入数据库，账号状态更新为「已注册」
```

### 关键技术点

| 特性 | 说明 |
|------|------|
| **协议主流程** | `internal/codexreg/protocol.go`：bogdanfinn tls-client 完成 CSRF→authorize→邮箱 OTP→create_account→callback→session；官方 Sentinel 由 Node 假浏览器执行 SDK 签发，识别到旧伪 token 立即失败。CF challenge 会映射为临时/IP 风控错误 |
| **屏幕随机化** | 每次注册随机一套常见桌面分辨率写入会话画像，账号之间不共用同一屏幕指纹 |
| **网页会话持久化** | 保存 ChatGPT/OpenAI 域的完整 Cookie（含 domain/path/httpOnly/secure/expiry），可导出并恢复真正的 `chatgpt.com` 登录会话；access token 仅作为会话派生凭据保存 |
| **验证码自动读取** | 直接对接邮箱 API（Outlook/Gmail），每 5 秒轮询一次，无需人工复制粘贴 |
| **GeoIP 自动对齐** | 注册前检测代理 IP 归属地，自动设置匹配的语言 / 时区 / 坐标；注册 UA、出口 IP、地区、代理写入 `auth_data`，下游用号时可沿用 |
| **会话与 AT 双状态** | 页面分别显示“账号/长期凭据”与“当前 access token”；绿色 AT 只能来自真实 `/backend-api/conversation/init` 授权探针，Cookie 会话存在、JWT `exp` 未到或注册成功都不等于 AT 有效 |
| **IP 拦截识别** | Cloudflare challenge、auth 限流都识别为「出口 IP 被拦」，自动换住宅 IP 重试 |
| **官方 Sentinel** | Node 假浏览器执行官方 SDK；拿不到真实 token 或识别到旧伪 token 都会让注册失败 |
| **会话观察器（so）** | 官方 SDK 的收集器监听 keydown / pointermove / click / scroll / paste / input / wheel。假浏览器在页面就绪时启动它，并合成真人的输入事件；日志里会打印 `Session Observer so_window=… keydown=… pointermove=…`，零交互的快照会直接拒绝提交 |
| **环境指纹对齐** | dx 程序会量隐藏 div 的文字盒子、枚举 `localStorage` 键、读 `navigator` 原型等；假环境按真实 Chrome 实测数据补齐（`scripts/font_metrics.js`、`scripts/nav_keys.js`）。`t` 若只剩一条报错文本会直接失败，不提交 |
| **请求头对齐** | sec-ch-ua 的 GREASE 品牌按 Chrome 算法生成，只发 3 个低熵 Client Hints，HTTP/2 线上头的集合、取值与顺序逐项对齐真实 Chrome |
| **并发安全** | GPT 并发跟随设置里的「最大并发数」，默认 45 秒错峰启动，每个任务独立 TLS 会话和 Node Sentinel 进程 |

---

## 截图预览

| 仪表盘 | GPT 注册 |
|:---:|:---:|
| ![仪表盘](./screenshots/dashboard.png) | ![GPT 注册](./screenshots/accounts.png) |

| 执行日志 | 邮箱管理 |
|:---:|:---:|
| ![执行日志](./screenshots/accounts-log.png) | ![邮箱管理](./screenshots/mailboxes.png) |

| 邮件取件（自动读取验证码） |
|:---:|
| ![邮件取件](./screenshots/mailboxes-mail.png) |

---

## 🏗️ 项目架构

```
chatgpt-register/
├── main.go                  # 入口：Gin 路由注册 + 静态文件嵌入
├── internal/
│   ├── auth/                # JWT 鉴权服务（单 token、自动续期、落库）
│   ├── browserboot/         # Rod 浏览器生命周期（Adobe / Grok 用，GPT 注册不启动浏览器）
│   ├── chatgptauth/         # ChatGPT 凭据刷新、真实探针与安全出库
│   ├── codexreg/            # ChatGPT 纯协议注册
│   │   ├── protocol.go      # 协议注册（Chrome TLS 指纹直连 auth.openai.com）
│   │   ├── sentinel_browser.go # 官方 Sentinel 签发入口（只走 Node 假浏览器）
│   │   ├── access_token.go  # Web Session / OAuth 刷新与 backend-api 探针
│   │   ├── codex.go         # accessToken 元数据解析
│   │   ├── geoip.go         # IP 归属地检测（代理验证）
│   │   └── codexreg.go      # 注册任务入口（只走协议引擎）
│   ├── adobereg/            # Adobe(Firefly) 注册核心逻辑（独立浏览器自动化）
│   ├── adobeproducer/       # Adobe 批量注册调度器（复用邮箱池自动取码）
│   ├── db/                  # SQLite 数据库初始化（纯 Go 驱动，无需 CGO）
│   ├── emailalias/          # 邮箱别名生成（裂变子号）
│   ├── handlers/            # HTTP 接口层（Gin Handler）
│   │   ├── auth.go          # 登录 / 改密接口
│   │   ├── registration.go  # 账户 CRUD + 日志 + 截图接口
│   │   ├── adobe.go         # Adobe 注册 CRUD + 生产 + 三种 Cookie 导出
│   │   ├── produce.go       # 批量生产控制（启动 / 状态 / 停止）
│   │   ├── mailbox.go       # 邮箱 CRUD + 取件接口
│   │   ├── proxy.go         # 代理测试接口
│   │   └── settings.go      # 系统设置接口
│   ├── mailfetch/           # 邮件取件（自动读取验证码）
│   ├── models/              # GORM 数据模型（Admin / Registration / GrokRegistration / AdobeRegistration / Mailbox / Setting）
│   └── producer/            # 批量注册调度器（并发控制 + 裂变策略）
└── static/                  # 前端静态页面（嵌入二进制，无需 Web 服务器）
    ├── dashboard.html        # 仪表盘
    ├── accounts.html/js      # GPT 注册
    ├── mailboxes.html/js     # 邮箱管理
    ├── settings.html         # 系统设置
    ├── adobe.html/js         # Adobe(Firefly) 注册管理
    ├── login.html            # 登录页
    ├── layout.js             # 公共布局 / 侧边栏
    └── style.css             # 毛玻璃主题 CSS（35KB 精心打磨）
```

**技术栈：** Go · Gin · GORM · SQLite（纯 Go 驱动）· go-rod · JWT · 原生 H5

---

## 🚀 快速开始

GPT 注册需要 PATH 中可用的 Node.js，以及随程序分发的 `scripts/` 目录；浏览器注册渠道还需要 Chrome/Chromium。请按启用的渠道准备运行环境。

### 方式一：直接运行（推荐）

下载 Release 中对应系统的可执行文件，双击运行或：

```bash
# Windows
./chatgpt-register.exe

# Linux
./chatgpt-register-linux
```

浏览器打开 [http://localhost:9000](http://localhost:9000)

### 方式二：源码运行

```bash
git clone https://github.com/yourname/chatgpt-register
cd chatgpt-register
go run .
```

### 方式三：自行编译

```bash
# Windows
go build -o chatgpt-register.exe .

# Linux
GOOS=linux go build -o chatgpt-register-linux .
```

### 自定义端口

```bash
ADDR=8080 ./chatgpt-register.exe
```

> 数据保存在同目录 `adskull.db`，已加入 `.gitignore`，请勿提交。

---

## 🔐 登录

- 默认账号：`admin` / `admin123`
- 首次登录后请立即在「系统设置」修改密码（密码长度 > 6 位）

**JWT 安全机制：**
- Token 有效期 **24 小时**，签发超过 2 小时自动续期（响应头 `X-New-Token` 下发）
- Token 全局唯一：重新登录 / 改密 / 续期均会使旧 Token 立即失效
- Token 落库持久化，进程重启后无需重新登录

---

## 📋 功能说明

### 批量生产（核心功能）

1. 在「邮箱管理」导入邮箱（支持批量 CSV 导入）
2. 在「系统设置」配置并发数、裂变数量，以及各渠道独立的代理 API 和开关
3. 在「仪表盘」点击「生产」，设置目标数量，一键启动
4. 实时查看进度、成功率、执行日志和注册截图

**裂变策略：** 每个邮箱先注册母号（用邮箱本身地址），母号成功后用 plus addressing 别名（`email+001@…`）注册裂变子号，每个邮箱最多 `1 + 裂变数量` 个账号。注册失败自动补单直到达标。

### 邮箱管理

- 状态四态：`待验证 / 验证中 / 验证失败 / 已验证`
- 导入后由服务端 10 并发后台认证；关闭页面或重启服务后会自动继续
- 支持重新认证所选邮箱、仅认证失败邮箱或全部邮箱，无效凭据立即失败，临时网络错误自动重试
- 邮箱管理、GPT 注册和仪表盘列表均提供带二次确认的“全部删除”操作
- 「取件」弹窗：3 秒轮询实时收件，sandbox iframe 隔离展示邮件内容
- 支持 Outlook（需填 `client_id` + `refresh_token`）：自动识别 OAuth 权限并选择 Microsoft Graph 或 Outlook IMAP XOAUTH2，兼容两类刷新令牌

### Adobe 注册（Firefly 免费生图/生视频，独立模块）

与 ChatGPT / Grok 注册完全分开，独立页面「Adobe 注册」、独立数据表 `adobe_registrations`、独立路由 `/api/adobe/*`，注册目标为 Adobe 账号（免费即可用 Firefly 免费额度）。

- **自动流程**：打开 `account.adobe.com` → 创建账号（邮箱 + 随机密码）→ 填姓名/生日/地区 → 提交 → 打开 Firefly 触发邮箱验证码页 → 自动读取邮箱池验证码并填入。
- **验证码自动读取**：复用现有邮箱池（同 Grok 自动模式），按发件人/主题/正文特征筛出 Adobe 邮件并提取 6 位码，每条记录独立记录所用邮箱。
- **批量生产 / 单个注册 / 停止 / 日志 / 失败截图 / 删除**：与 Grok 页面一致。
- **Cookie 导出（三选一，导出即出库）**：
  - **Cookie 字符串**：`k=v; k=v; ...`（单账号 `.txt`，多账号打包 `.zip`）
  - **Cookie JSON 对象**：单个 Adobe 的 Cookie 对象（含 `cookie_string` / `cookies_map` / 带元数据的 `cookies` / `storage`；单账号 `.json`，多账号 `.zip`）
  - **Cookie 数组（多 Adobe 批量）**：所选账号合并为单个 `.json` 数组
- **安全**：列表接口不返回 `auth_data`（`json:"-"`），日志与截图不含验证码或 Cookie 明文；Cookie 仅通过上述导出接口、且仅对已注册记录开放。
- **相关设置键**：`adobe_headless`（无头）；并发跟全局 `max_concurrency`，代理使用「渠道代理」里的 Adobe 独立配置。

---

## ⚙️ 使用指南

### 第一步：导入邮箱

进入「邮箱管理」，支持两种方式导入：

- **手动添加**：填写邮箱地址、密码、服务商
- **批量导入**：点击「批量导入邮箱」，每行一条，格式：
  ```
  email----password----provider
  ```
  `provider` 支持 `outlook` / `hotmail` / `gmail` 等

> Outlook 邮箱需额外填写 `client_id` 和 `refresh_token`。系统优先按 Microsoft Graph `.default` 刷新令牌；旧式令牌不接受该 scope（`AADSTS90023`）时自动按原授权刷新，并通过 Outlook IMAP XOAUTH2 收件，无需手动选择协议。

---

### 第二步：配置系统设置

进入「系统设置」，配置以下参数后保存：

| 参数 | 说明 | 建议值 |
|------|------|--------|
| 并发数 | 同时注册的账号数量，GPT 按这个数字执行 | 4～6 |
| 裂变数量 | 每个邮箱注册的子号数 | 5（即 1母 + 5子 = 6个账号）；`+别名` 子号与母号天然可被关联，追求存活率时建议调低甚至设为 0 |
| 注册错峰间隔 | 相邻两个 GPT 任务实际启动的最小间隔 | 45 秒 |
| 渠道代理 | GPT / Grok / Adobe / Leonardo 各自配置开关和提取 API | 低并发建议 `count=1` 或小批量；API 使用粘性会话 |

在「渠道代理」选择渠道，填写完整提取 API，点击「提取并测试」，然后保存此渠道。测试使用当前草稿，不会自动保存或启用。开关关闭时新任务直连；开启后提取失败会明确报错，不会自动直连。已运行任务沿用已分配的会话，保存配置仅清理该渠道尚未分配的缓存。

BestGo 示例按钮会填入该渠道的示例 URL。`region` 控制地区，`count` 控制提取数量，`sessTime` 是出口轮换分钟数；GPT 按 8 分钟、其余渠道按 5 分钟流程预算检查，并额外预留 30 秒。缓存仅保留 30 秒；API 返回的 session、地区及其他认证参数原样保留，API 会话只分配一次。轮换计时暂按提取请求发出时保守估算，供应商的实际起算规则需以实测为准。

旧全局列表首次启动时会复制到四个渠道，来源显示为「独立列表（兼容旧配置）」，迁移后不会覆盖独立配置。切换为 API 后只使用该渠道的提取结果，旧全局设置仅作历史配置保留。API 提示 IP 白名单失败时，需在供应商后台放行运行服务的服务器出口 IP。

**代理格式：**
```
http://user:pass@ip:port
socks5://user:pass@ip:port
http://ip:port
```

---

### 第三步：启动批量生产

1. 进入「仪表盘」，点击右上角「**空跑**」按钮先测试环境
2. 点击「**生产**」，输入目标账号数量
3. 系统自动调度：优先注册母号 → 母号成功后裂变子号 → 失败自动补单直到达标
4. 实时查看成功数 / 失败数 / 进度条

---

### 查看注册详情

- 进入「GPT 注册」点击任意账号可查看**实时执行日志**（步骤级别，精确到秒）
- 点击「截图」可查看注册过程中的**浏览器截图**，方便排查失败原因
- 支持按状态筛选：待注册 / 注册中 / 已注册 / 注册失败
- 支持导出 ChatGPT 网页 Cookie 会话、Sub2API 聚合 JSON，或 CLIProxyAPI（CPA）auth-dir 格式。导出直接使用库存里的 access token 和网页 Cookie，不在下载时刷新或探针。已撤销、需重新登录、JWT 已过期的号不进文件

---

## ❓ 常见问题

**Q：浏览器第一次启动很慢？**
> A：首次运行会自动下载 Chromium（约 150MB），下载完成后后续启动秒开。

**Q：注册失败怎么办？**
> A：系统会自动重试补单，无需手动干预。查看执行日志可定位具体失败原因（如验证码超时、IP 被封等）。

**Q：不配置代理可以用吗？**
> A：可以，在「渠道代理」关闭对应渠道的开关即可直连。开启代理后必须配置有效来源；空列表或 API 失败会报错。

**Q：能不能完全不开浏览器做纯协议注册？**
> A：主流程始终是 HTTP 客户端。Sentinel 不再本地拼接，也不再启动 Chromium：用 Node 假 Windows Chrome 146 环境执行官方 SDK，网络回调到同一条 TLS。无法取得真实 token 时注册失败，不会用伪 token 降级。

**Q：注册成功的号几分钟后就 401 `token_revoked`，网页 Cookie 却还能换出 session？**
> A：这是注册时上报的浏览器画像被异步风控判为“无人操作/非真实浏览器”，约 6~16 分钟后整张 AT 被撤（`me` / `accounts/check` / `conversation/init` 同时 401），网页会话仍会把同一张已撤 AT 再吐一遍。真实 Chrome 对照实验证明决定因素是官方 Sentinel 的 Session Observer 里有没有真实用户交互，以及 `t`（dx 环境指纹）是否完整；RUM / Cloudflare JSD / ces 遥测和登录后的页面心跳都不是必需项。当前版本已在假浏览器里补齐这两项，并用 `keydown` / `pointermove` 数量与 `t` 长度做失败即关闭校验。若日志出现 `sentinel_so_empty` / `sentinel_t_error`，说明官方 SDK 改版后假环境又缺了 API，用 `scripts/lab/lab_driver.js` 复现并补齐。

**Q：注册出来的号一用（生图）就被封 / 还没用就死了？**
> A：封号几乎都是"关联"问题，而不是单个号的行为。请逐项对照：
> 1. **注册指纹**：协议引擎用 Chrome 146 PSK TLS/HTTP2 指纹；HTTP UA/Client Hints 与该指纹对齐；关键动作的 token 来自 Node 里执行的官方 SDK，不是本地拼接。
> 2. **测活**：旧版测活用无 Cookie 的新浏览器请求 `/backend-api/me`，会制造假 401；本版本恢复账号 Cookie、注册 UA、屏幕、时区和原粘性代理 session，并由网页自身验证 `/api/auth/session`。
> 3. **用号方式**：`auth_data` 里带有注册时的 `user_agent` / `screen` / `registered_ip` / `registered_country` / `registered_timezone` / `proxy`。下游网关应沿用同一地区和代理线路，不要用一台服务器的固定 IP 集中调用大量账号。
> 4. **裂变子号**：`a+001@…`、`a+002@…` 与母号是同一个邮箱，OpenAI 一眼就能关联；母号被封时子号大概率跟着走。看重存活率就把裂变数量调低。
> 5. **节奏**：免费号有很低的生图配额，新号第一天就高频生图会立刻触发风控；建议养号（先正常聊几轮）、分散使用时间、单号限速。

**Q：账号导出格式是什么？**
> A：在「GPT 注册」勾选账号后可选择“导出网页会话”“导出 Sub2API”或“导出 CPA”。网页会话包含可恢复到 `chatgpt.com` 的 Cookie、Cookie Header、注册 UA、屏幕、时区和代理信息；单账号为 JSON，多账号为 ZIP。导出直接写入库存凭据，不走代理。已撤销、需重新登录、JWT 已过期的号不进文件。要确认号是否还能用，用账号页的测活。

---

## ⭐ 如果觉得好用，欢迎 Star！
