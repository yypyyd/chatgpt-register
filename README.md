# chatgpt-register

> **ChatGPT 账号全自动批量注册管理台** · 无头浏览器全自动 · 30 秒极速注册 · 百分百成功率 · 一键裂变子号

---

🌐 **生图站** [vividai.run](https://vividai.run) &nbsp;|&nbsp;
👥 **QQ 交流群** [1106849765](https://qm.qq.com/q/1106849765) &nbsp;|&nbsp;
🐧 **QQ** 1114639355 &nbsp;|&nbsp;
🛒 **小店** [pay.ldxp.cn/shop/chiyi](https://pay.ldxp.cn/shop/chiyi) &nbsp;|&nbsp;
✉️ **邮箱** [vividairun@gmail.com](mailto:vividairun@gmail.com)

---

## ✨ 核心优势

| 🚀 30 秒极速注册 | ✅ 百分百成功率 | 🔁 母号裂变子号 |
|:---:|:---:|:---:|
| Rod 浏览器自动化 + Stealth 反检测，全程无需人工干预 | 验证码自动从邮箱读取，全流程零手动操作 | 每个邮箱注册 1 个母号 + N 个别名子号，账号数量指数级增长 |

| 🌐 代理池轮转 | 📊 可视化管理台 | 📦 零依赖部署 |
|:---:|:---:|:---:|
| 内置代理池按账号轮转，多 IP 并发注册不封号 | 毛玻璃风格 UI，实时仪表盘 + 执行日志可视化 | 纯 Go 编译单文件，无需安装任何环境，下载即用 |

---

## 🤖 无头注册——技术亮点

> 基于 **go-rod** 驱动本机真实 Chrome（new headless），不改浏览器原生指纹，只修正无头标记并对齐出口地区；键盘/鼠标全部走真实输入事件，模拟真人操作全程自动完成注册。

### 注册全流程（全自动，无需人工）

```
查出口 IP 归属地 → 按地区决定语言 / 时区
    ↓
启动本机 Chrome（new headless，去自动化参数，随机屏幕规格，真实 UA + Client Hints）
    ↓
打开 ChatGPT 注册页，逐键输入邮箱 + 随机密码，真实鼠标点击提交
    ↓
实时监听邮箱，自动读取 6 位验证码并填入（最长等待 3 分钟）
    ↓
完成注册 → 进入主界面 → 预热：发一条普通对话并等待回复（可关）
    ↓
验证真实网页对话，保存登录 Cookie 与 accessToken，并记录注册 UA / 出口 IP / 地区 / 代理（不注册 Agent Identity）
    ↓
导出 ChatGPT 账号凭据 JSON
    ↓
写入数据库，账号状态更新为「已注册」
```

### 关键技术点

| 特性 | 说明 |
|------|------|
| **原生指纹** | 不注入 stealth 脚本、不写死 UA：用本机真实 Chrome 的 UA / WebGL / 插件 / Client Hints，只把无头标记（`HeadlessChrome`）换回正常品牌，避免 "UA 说 Chrome 150、Client Hints 为空、WebGL 是 Mac 显卡" 这类互相矛盾的指纹 |
| **真人交互** | 键盘逐键 keydown/keyup（Shift 字符带修饰键）、鼠标带轨迹移动后再点击（`isTrusted=true`）、步骤间随机停顿；不用 `insertText` 和 `element.click()` |
| **屏幕随机化** | 每次注册随机一套常见桌面分辨率，并还原"屏幕 > 窗口 > 视口"的层次，账号之间不共用同一屏幕指纹 |
| **注册后预热** | 注册成功先在同一浏览器、同一出口 IP 里发一条普通问题并等回复；开启时预热是成功关卡，不能完成网页对话的账号不会进入可用库存 |
| **网页会话持久化** | 在销毁临时浏览器前保存 ChatGPT/OpenAI 域的完整 Cookie（含 domain/path/httpOnly/secure/expiry），可导出并恢复真正的 `chatgpt.com` 登录会话；access token 仅作为会话派生凭据保存 |
| **验证码自动读取** | 直接对接邮箱 API（Outlook/Gmail），每 5 秒轮询一次，无需人工复制粘贴 |
| **GeoIP 自动对齐** | 注册前检测代理 IP 归属地，自动设置匹配的浏览器语言 / 时区 / 坐标；注册 UA、出口 IP、地区、代理写入 `auth_data`，下游用号时可沿用 |
| **测活不串号** | 测活为每个账号单独一个浏览器上下文、单独走代理出口，恢复该账号 Cookie 后验证 `/api/auth/session`；不再把 token 塞进无 Cookie 的新浏览器请求 `/backend-api/me` 造成 401 误判 |
| **共享进程池** | 多个账号共用一个 Chrome 进程、各自独立 BrowserContext（cookie / 代理出口 / 窗口尺寸 / 屏幕 / 语言互相隔离），每号省 150~300MB 内存与 1~3 秒启动，上下文分配约 2ms；进程按时长 / 累计账号数自动退役重启 |
| **IP 拦截识别** | Cloudflare 整页人机验证、提交邮箱后服务端无响应都识别为「出口 IP 被拦」，自动换住宅 IP 重试，不再当成邮箱失败白等 60 秒进冷却 |
| **浏览器选择** | 默认优先本机安装的 Chrome/Edge（最新版、真实品牌）；没有则回退到自动下载的 Chromium |
| **无头模式** | 生产环境开启无头模式，无需显示器，支持服务器 / VPS 部署 |
| **截图存证** | 注册每个关键步骤自动截图，失败时可直接在管理台查看现场图，快速定位问题 |
| **并发安全** | 多个注册任务并发执行，每个任务独立浏览器上下文，互不干扰 |

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
│   ├── browserboot/         # Rod 浏览器生命周期管理（启动时自动下载 Chromium）
│   ├── codexreg/            # ChatGPT 注册核心逻辑（浏览器自动化 + Stealth）
│   │   ├── browser.go       # 浏览器实例封装
│   │   ├── codex.go         # 注册流程自动化
│   │   ├── geoip.go         # IP 归属地检测（代理验证）
│   │   └── codexreg.go      # 注册任务入口
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

**技术栈：** Go · Gin · GORM · SQLite（纯 Go 驱动）· go-rod · rod/stealth · JWT · 原生 H5

---

## 🚀 快速开始

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
2. 在「系统设置」配置并发数、裂变数量、代理池
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
- **相关设置键**：`adobe_headless`（无头）；并发跟全局 `max_concurrency`，代理跟全局 `proxy_enabled` / `proxy_list`。

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
| 并发数 | 同时注册的账号数量 | 3 ~ 5 |
| 裂变数量 | 每个邮箱注册的子号数 | 5（即 1母 + 5子 = 6个账号）；`+别名` 子号与母号天然可被关联，追求存活率时建议调低甚至设为 0 |
| 无头模式 | 是否隐藏浏览器窗口 | 生产环境建议开启 |
| 代理池 | 每行一个代理，格式见下方 | 建议动态住宅代理，每号独立出口 |
| GPT 注册后预热对话 | 注册成功后先发一条普通对话再取 token（`chatgpt_warmup`） | 开启 |
| GPT 注册浏览器 | 留空优先本机 Chrome；`rod` 用内置 Chromium；或填路径（`chatgpt_browser_bin`） | 留空 |
| GPT 共享浏览器进程池 | 多账号共用 Chrome 进程、独立上下文（`chatgpt_browser_pool`） | 开启 |
| 每进程账号数 | 一个 Chrome 进程同时承载的账号数（`chatgpt_contexts_per_host`） | 4；16 核 16G 机器并发 8 时可设 4~8 |

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
- 支持导出 ChatGPT 网页 Cookie 会话、Sub2API 聚合 JSON，或 CLIProxyAPI（CPA）auth-dir 格式；网页会话和 CPA 多账号导出均为 ZIP

---

## ❓ 常见问题

**Q：浏览器第一次启动很慢？**
> A：首次运行会自动下载 Chromium（约 150MB），下载完成后后续启动秒开。

**Q：注册失败怎么办？**
> A：系统会自动重试补单，无需手动干预。查看执行日志可定位具体失败原因（如验证码超时、IP 被封等）。

**Q：不配置代理可以用吗？**
> A：可以，留空即直连。但大量并发注册建议配置代理池，避免 IP 被限流。

**Q：能不能做成纯协议注册（不开浏览器）？**
> A：ChatGPT 注册链路（`auth.openai.com`）每一步都要带 OpenAI Sentinel 头：VM 级混淆 JS 产出的浏览器指纹载荷 + PoW + Turnstile（有时叠 Arkose），脚本几周换一版；外层 Cloudflare 还校验 TLS/HTTP2 指纹 ↔ UA ↔ Client Hints ↔ Sentinel 载荷是否自洽。纯协议 = 常态化跟版，且协议号在 OpenAI 那边的画像（tls-client 指纹 + 零前端遥测）正是批量封号的画像。本项目选的路线是「真实 Chrome + 共享进程池」：一个进程带多个隔离上下文，资源接近协议方案，指纹和行为仍是真人级。Grok 之所以能纯协议，是因为 x.ai 只有一层 Turnstile。

**Q：注册出来的号一用（生图）就被封 / 还没用就死了？**
> A：封号几乎都是"关联"问题，而不是单个号的行为。请逐项对照：
> 1. **注册指纹**：本版本已改为本机真实 Chrome + 真实 Client Hints + 真人输入事件。
> 2. **测活**：旧版测活用无 Cookie 的新浏览器请求 `/backend-api/me`，会制造假 401；本版本恢复账号 Cookie、注册 UA、屏幕、时区和原粘性代理 session，并由网页自身验证 `/api/auth/session`。
> 3. **用号方式**：`auth_data` 里带有注册时的 `user_agent` / `screen` / `registered_ip` / `registered_country` / `registered_timezone` / `proxy`。下游网关应沿用同一地区和代理线路，不要用一台服务器的固定 IP 集中调用大量账号。
> 4. **裂变子号**：`a+001@…`、`a+002@…` 与母号是同一个邮箱，OpenAI 一眼就能关联；母号被封时子号大概率跟着走。看重存活率就把裂变数量调低。
> 5. **节奏**：免费号有很低的生图配额，新号第一天就高频生图会立刻触发风控；建议养号（先正常聊几轮）、分散使用时间、单号限速。

**Q：账号导出格式是什么？**
> A：在「GPT 注册」勾选账号后可选择“导出网页会话”“导出 Sub2API”或“导出 CPA”。网页会话包含可恢复到 `chatgpt.com` 的 Cookie、Cookie Header、注册 UA、屏幕、时区和代理信息；单账号为 JSON，多账号为 ZIP。2026-09-06 以前的旧记录只保存了 access token，没有 Cookie，需重新登录或重新注册才能恢复网页会话。用于网页生图时请使用网页会话导出，并恢复结构化 Cookie 与注册现场，不要把 access token 直接请求 `api.openai.com/v1`。

---

## ⭐ 如果觉得好用，欢迎 Star！
