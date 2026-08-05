package grokreg

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"chatgpt-register/internal/grokreg/clearance"
	"chatgpt-register/internal/grokreg/oauth"
	"chatgpt-register/internal/grokreg/protocol"
)

// registerProtocol drives the whole Grok signup over HTTP/gRPC using a
// Chrome-impersonating TLS client. A browser is spawned only to mint the
// single-use Cloudflare Turnstile token (via the CloakBrowser helper), which
// exits as soon as the token is issued — everything else is protocol traffic.
func registerProtocol(ctx context.Context, in Input) (*Result, error) {
	in.logf("Grok 协议注册启动（浏览器仅用于签发 Turnstile 令牌）")
	if strings.TrimSpace(in.Proxy) != "" {
		server, _, _, perr := parseProxy(in.Proxy)
		if perr != nil {
			return nil, fmt.Errorf("解析代理失败: %w", perr)
		}
		in.logf("Grok 协议客户端使用上游代理: %s", server)
	}

	impersonate := firstNonEmpty(in.Impersonate, "chrome_131")
	fallbacks := protocol.FallbackProfiles(in.ImpersonateFallback)
	fsURL := strings.TrimSpace(in.FlareSolverrURL)

	client, err := protocol.NewClientOpts(protocol.ClientOptions{
		Proxy:               in.Proxy,
		Impersonate:         impersonate,
		ImpersonateFallback: fallbacks,
	})
	if err != nil {
		return nil, fmt.Errorf("创建协议客户端失败: %w", err)
	}

	// FetchConfig scrapes sitekey / Next-Action id / router state tree at runtime.
	// Protocol-first: on Cloudflare block try fingerprint fallbacks, then (when
	// configured) fall back to a FlareSolverr clearance bundle.
	var cm *clearance.Manager
	scfg, err := client.FetchConfig()
	if err != nil {
		in.logf("⚠ 首选指纹 warm 失败(profile=%s): %v，尝试指纹回退", client.Profile(), err)
		tried := map[string]struct{}{client.Profile(): {}}
		for _, fb := range fallbacks {
			if _, ok := tried[fb]; ok {
				continue
			}
			tried[fb] = struct{}{}
			if rerr := client.RecreateWithProfile(fb); rerr != nil {
				continue
			}
			in.logf("尝试指纹回退 profile=%s", fb)
			if scfg, err = client.FetchConfig(); err == nil {
				in.logf("warm 成功 profile=%s", client.Profile())
				break
			}
			in.logf("回退 %s 仍失败: %v", fb, err)
		}
	}
	if err != nil && fsURL != "" {
		in.logf("⚠ 协议直连仍被 Cloudflare 拦截，启用 FlareSolverr clearance 兜底")
		cm = clearance.NewManager(fsURL, strings.TrimSpace(in.ClearanceProxy), in.ClearanceURLs)
		if msg, perr := cm.Prewarm(); perr != nil {
			in.logf("clearance 预热异常: %v | %s", perr, msg)
		} else {
			in.logf("clearance 预热完成: %s", msg)
		}
		client, err = protocol.NewClientOpts(protocol.ClientOptions{
			Proxy:       in.Proxy,
			Clearance:   cm,
			Impersonate: client.Profile(),
		})
		if err != nil {
			return nil, fmt.Errorf("创建协议客户端(clearance)失败: %w", err)
		}
		scfg, err = client.FetchConfig()
	}
	if err != nil {
		return nil, fmt.Errorf("获取注册配置失败: %w", err)
	}
	in.logf("注册配置就绪 site_key=%s action=%s… source=%s profile=%s",
		scfg.SiteKey, trimText(scfg.ActionID, 12), scfg.Source, client.Profile())

	// Chromium cannot authenticate to an upstream proxy via --proxy-server, so an
	// authenticated proxy needs a loopback bridge for the Turnstile mint. The TLS
	// client itself talks to the authenticated proxy directly.
	in.mintProxy = normalizeProxy(in.Proxy)
	if bridge, addr := maybeAuthBridge(in.Proxy); bridge != nil {
		defer bridge.Close()
		in.mintProxy = addr
	}

	// 1) request email validation code, wait for the mailbox to deliver it.
	if err := client.CreateEmailCode(in.Email); err != nil {
		return nil, fmt.Errorf("发送邮箱验证码失败: %w", err)
	}
	in.logf("已请求邮箱验证码，等待收码…")
	code, err := in.WaitCode(ctx)
	if err != nil {
		return nil, fmt.Errorf("等待验证码失败: %w", err)
	}
	code = strings.TrimSpace(code)

	client.ClearAuthCookies()
	if err := client.VerifyEmailCode(in.Email, code); err != nil {
		return nil, fmt.Errorf("校验邮箱验证码失败: %w", err)
	}
	// ValidatePassword mirrors the browser form's field 4/5 probe; non-fatal.
	if err := client.ValidatePassword(in.Email, in.Password); err != nil {
		in.logf("ValidatePassword 跳过/失败(非致命): %v", err)
	}

	// 2) mint a Turnstile token (the only browser step) then submit signup.
	token, err := mintTurnstileToken(ctx, in, scfg.SiteKey, protocol.SiteURL+"/sign-up")
	if err != nil {
		return nil, fmt.Errorf("签发 Turnstile 令牌失败: %w", err)
	}
	in.logf("Turnstile 令牌已签发(len=%d)，浏览器已退出，转协议注册", len(token))

	body := protocol.BuildSignupBody(in.Email, in.Password, code, token)
	text, sso, err := client.SignupServerAction(body, scfg.ActionID, scfg.StateTree)
	if sso == "" {
		sso = protocol.ExtractSSOFromText(text)
	}
	if err != nil && sso == "" {
		return nil, fmt.Errorf("协议注册失败: %w", err)
	}
	if sso == "" {
		return nil, fmt.Errorf("协议注册未返回会话 SSO")
	}
	in.logf("注册成功，已获得会话 SSO")

	// 3) a fresh password login SSO is more reliable for device authorization
	// than the signup Server Action SSO; keep the latter as fallback.
	if fresh, ferr := createFreshSessionSSOProto(ctx, in, client, cm, scfg); ferr != nil {
		in.logf("CreateSession 获取新 SSO 失败，沿用注册 SSO: %v", ferr)
	} else if fresh != "" {
		sso = fresh
		in.logf("CreateSession 已换取新会话 SSO")
	}

	// 4) OAuth device flow → CLIProxyAPI (CPA) xAI credential.
	var cpaXAI map[string]any
	if oauthClient, oerr := oauth.NewClient(in.Proxy, cm, 0); oerr != nil {
		in.logf("创建 OAuth 客户端失败(跳过 CPA 凭证): %v", oerr)
	} else {
		// Brand-new SSO is occasionally rejected by auth.x.ai device verify; settle.
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
		cred, exErr := oauthClient.ExchangeWithFlow(ctx, sso, func(f oauth.DeviceFlow) {
			in.logf("OAuth 设备码流程 user_code=%s", f.UserCode)
		})
		if exErr != nil {
			in.logf("⚠ OAuth 设备授权失败(CPA 凭证缺失): %v", exErr)
		} else {
			cpaXAI = buildCPAXAIAuth(in.Email, xaiTokenResult{
				AccessToken:  cred.AccessToken,
				RefreshToken: cred.RefreshToken,
				IDToken:      cred.IDToken,
				TokenType:    cred.TokenType,
				ExpiresIn:    cred.ExpiresIn,
			}, cred.TokenEndpoint)
			in.logf("CPA xAI 凭证已铸造")
		}
	}

	auth := map[string]any{
		"auth_mode":   "grok_protocol_session",
		"platform":    "grok",
		"email":       in.Email,
		"password":    in.Password,
		"first_name":  in.FirstName,
		"last_name":   in.LastName,
		"captured_at": time.Now().UTC().Format(time.RFC3339),
		"sso":         sso,
		"cookies": []map[string]any{{
			"name":   "sso",
			"value":  sso,
			"domain": ".x.ai",
			"path":   "/",
		}},
	}
	if cpaXAI != nil {
		auth["cpa_xai"] = cpaXAI
	}
	return &Result{AuthJSON: auth}, nil
}

// createFreshSessionSSOProto performs a password login (WarmSignin → fresh
// Turnstile token → CreateSession) to obtain a session SSO minted the same way
// grok.com issues it after sign-in.
func createFreshSessionSSOProto(ctx context.Context, in Input, base *protocol.Client, cm *clearance.Manager, scfg protocol.SignupConfig) (string, error) {
	var last error
	for attempt := 1; attempt <= 2; attempt++ {
		login, err := protocol.NewClientOpts(protocol.ClientOptions{
			Proxy:       in.Proxy,
			Clearance:   cm,
			Impersonate: base.Profile(),
			Timeout:     45 * time.Second,
		})
		if err != nil {
			return "", err
		}
		if _, err = login.WarmSignin(); err != nil {
			last = err
			continue
		}
		token, terr := mintTurnstileToken(ctx, in, scfg.SiteKey, protocol.SigninURLGrok)
		if terr != nil {
			last = terr
			continue
		}
		fresh, serr := login.CreateSession(strings.ToLower(strings.TrimSpace(in.Email)), in.Password, token)
		if serr == nil && fresh != "" {
			return fresh, nil
		}
		last = serr
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(1200 * time.Millisecond):
		}
	}
	if last == nil {
		last = fmt.Errorf("CreateSession 未返回 SSO")
	}
	return "", last
}

// maybeAuthBridge starts a loopback proxy bridge only when the upstream proxy
// carries credentials (Chromium can use an unauthenticated proxy directly).
func maybeAuthBridge(raw string) (*localAuthProxyBridge, string) {
	if strings.TrimSpace(raw) == "" {
		return nil, ""
	}
	u, err := url.Parse(normalizeProxy(raw))
	if err != nil || u.User == nil {
		return nil, ""
	}
	if pass, hasPass := u.User.Password(); !hasPass && pass == "" && u.User.Username() == "" {
		return nil, ""
	}
	bridge, addr, berr := startLocalAuthProxyBridge(raw)
	if berr != nil {
		return nil, ""
	}
	return bridge, addr
}
