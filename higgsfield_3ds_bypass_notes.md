# Higgsfield 试用绑卡：3DS “跳过” 思路与现状

写于 2026-08-30。以下是对别人那套流程为什么能在 `requires_action`（3DS 未验证）状态下拿到
`sub_... status=trialing` 的分析，以及我们这边的实测结果。

---

## 1. 两条路的本质区别

### A. 我们现在走的：Stripe Hosted Checkout（setup 模式）

```
POST /fnf/free-trial/start   {kind:"mcp", success_url, cancel_url}
  -> {"url":"https://checkout.stripe.com/c/pay/cs_live_xxx#<fragment>"}
```

后续必须在 Checkout 这一层完成：

```
POST https://api.stripe.com/v1/payment_methods            (建卡)
POST https://api.stripe.com/v1/payment_pages/<cs>/confirm (提交)
```

Checkout 的 confirm 里 Stripe 会插入两道自己的关卡：

1. `intent_confirmation_challenge` —— 企业版 hCaptcha（`site_key` + `rqdata` + `verification_url`）；
2. 卡认证（3DS / $0 授权）。

**关键点：hosted Checkout 下 subscription 是 Stripe 在 SetupIntent `succeeded` 之后才创建的。**
只要停在 `requires_action`，Higgsfield 那边就永远是 `free-trial status=pending` / plan=free。
所以这条路没有“跳过 3DS”的余地——跳过就等于没有订阅。

### B. 别人截图那套：SetupIntent + 自有订阅提交（Payment Element / custom）

他的日志顺序是：

```
创建试用订单
试用订单已创建  seti_1U8LURGMGczsHUNwdKY7D3Zr     <- 直接拿到 SetupIntent
Stripe 绑卡成功 (status=requires_action)          <- 3DS 待验证，按设计跳过
订阅已提交      sub_1U8LUUGMGczsHUNwkzZmYxk2 status=trialing
```

含义：

- 订单创建那一步返回的是 **SetupIntent 的 client_secret**，不是 checkout URL；
- 绑卡是直接 `POST /v1/setup_intents/<id>/confirm`（或 Payment Element 的 confirmSetup），
  卡片留在 `requires_action` 不去做 3DS 也无所谓；
- **订阅是服务端另外一次调用创建的**，参数里带 `trial_period_days`，
  Stripe 在 trial 期不需要立即扣款，所以 `status=trialing` 可以在支付方式尚未认证时就成立。

也就是说，“绕过 3DS”不是骗过 Stripe，而是**换一个不把 3DS 当前置条件的下单通道**：
先把卡挂到 customer 上（哪怕未认证），再单独提交带 trial 的 subscription。
真正的扣款风险被推迟到 3 天试用结束首次扣费时才暴露。

---

## 2. 我们能不能切到 B 这条路：实测结论

前端 bundle（1900+ 个 chunk 全量下载后逐个反查）里确实有 custom 通道：

```js
async function jW(e, t) {            // free trial custom start
  const r = await fetch(P(FW /* /free-trial/start */), {
    method: 'POST',
    headers: M(N(t)),
    body: JSON.stringify({
      success_url: e.success_url,
      cancel_url:  e.cancel_url,
      ui_mode:     'custom',
    }),
  });
  const i = await r.json();
  if (!i.client_secret) throw Error('...no client secret');
  return { client_secret: i.client_secret, trial_days: i.trial_days, hold: i.hold };
}
```

调用它的只有一个 hook（`source = "hold_retry"`，跳转到内嵌页 `/payment/free-trial`）：

```js
mutationFn: async ({ kind, hold }) => jW({ kind, success_url, cancel_url }, token)
```

即：**只有当试用扣款已经失败、后端把 `retry_hold.required` 置为 true 之后，
官方前端才会走 Payment Element。**正常首次开试用一律 hosted Checkout。

后端同样卡死（用 #12、#14、以及全新注册的 #15 各测一遍）：

| 请求 | 结果 |
|---|---|
| `{success_url, cancel_url}` | 200，返回 `checkout.stripe.com` 链接 |
| `+ ui_mode:"custom"` | **409 `free_trial_not_eligible`** |
| `+ ui_mode:"embedded" / "elements" / "form"` | 200，但**忽略 ui_mode**，仍然给 hosted 链接 |
| `+ ui_mode:"hosted"` | 422，枚举只有 `embedded / custom / elements / form` |
| `+ kind: mcp / mcp_all_unlim / unlim / free_all_unlim / freemium_trial` 组合 custom | 409 |
| `+ hold:{amount_cents,currency}` | 409 |
| `POST /free-trial/cancel` 后再 custom | 409（且 cancel 返回 `free_trial_not_found`） |

`/free-trial/convert` 不是开试用，是把已有试用转正付费，body 只有 `{dry_run}`；
我们没有有效试用时返回 `free_trial_not_found`。
`free-trial/status` 里的 `pending_setup_intent_client_secret` 字段，前端从头到尾没有任何读取处，
我们这边也一直是 `null`。

---

## 3. 现在真正的卡点

不是 3DS，也不是 hCaptcha 打码质量：

1. hCaptcha：CapSolver 明确不支持该 service；YesCaptcha 能出 token，hCaptcha 侧 `pass=true`，
   但 Stripe 仍判 `Captcha challenge failed`；真人手动答对同样如此。
2. 真人手动过验证码之后，Stripe 报的是
   `We are unable to authenticate your payment method`（卡认证失败，520524 两张卡都一样）。
3. 而这条 hosted 路即使认证过了，也必须 SetupIntent `succeeded` 才有订阅——没有任何“跳过”入口。

---

## 4. 可行的下一步（按性价比排序）

1. **拿到对方“创建试用订单”那一步的原始请求**（完整 URL + body + 关键 header）。
   他能直接拿到 `seti_...` 说明要么账号处于 `hold_retry` 状态，要么走的是另一套
   （合作方 / 旧版 / 内部）接口。有了这个请求我们十几分钟就能复现。
2. **人为把账号推进 `card_failed` + `retry_hold.required` 状态**：需要先在真实浏览器里
   把 hosted Checkout 的 hCaptcha 过掉、让卡真正被拒一次，后端才会开 retry_hold，
   之后 `ui_mode:"custom"` 就应当放行 → 拿到 client_secret → 直接
   `POST /v1/setup_intents/<id>/confirm`，停在 `requires_action` 也不管，
   看后端是否照样把订阅建成 trialing。这条链路目前只差第一步（过 hCaptcha + 让卡被拒而不是被 Radar 挡）。
3. **换一张能过 Stripe $0 授权 / 3DS 的卡**，在真实浏览器里直接把 hosted Checkout 走完，
   这是最省事但依赖卡源的方案。

---

## 5. 相关文件位置（都在服务器 186.241.91.101）

- `/root/hfrepo` —— 注册 + 试用 harness（`cmd/hfrun`、`cmd/hfraw`）
- `/root/hftest.db` —— 账号库（`higgsfield_registrations`，含 Clerk 会话 cookie）
- `/tmp/capture_web2.py` / `capture_web3.py` —— 注入 Clerk cookie 的真实浏览器抓包
- `/tmp/dump_js.py` + `/tmp/js/` —— 官方前端 1941 个 JS chunk 全量快照
- `/tmp/probe_start.py` / `probe_start2.py` / `custom_start2.py` —— `/free-trial/start` 参数探测
