package adobereg

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"chatgpt-register/internal/proxyutil"
)

const (
	imsTokenCheckURL = "https://adobeid-na1.services.adobe.com/ims/check/v6/token?jslVersion=v2-v0.60.0-10-g3e7a9f3"
	imsTokenScope    = "AdobeID,firefly_api,openid,pps.read,pps.write,additional_info.projectedProductContext," +
		"additional_info.ownerOrg,uds_read,uds_write,ab.manage,read_organizations,additional_info.roles," +
		"account_cluster.read,creative_production,profile"
)

// httpClientWithProxy 按代理串构造 HTTP 客户端（代理为空则直连）。
func httpClientWithProxy(proxy string) *http.Client {
	client := &http.Client{Timeout: 120 * time.Second}
	if t, err := proxyutil.Transport(proxy); err == nil && t != nil {
		client.Transport = t
	}
	return client
}

// imsAccessToken 用会话 Cookie 换 IMS access_token（下游 image2api 也是这么换的）。
func imsAccessToken(ctx context.Context, proxy, cookie string) string {
	form := url.Values{}
	form.Set("client_id", "clio-playground-web")
	form.Set("guest_allowed", "true")
	form.Set("scope", imsTokenScope)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, imsTokenCheckURL, strings.NewReader(form.Encode()))
	if err != nil {
		return ""
	}
	req.Header.Set("content-type", "application/x-www-form-urlencoded;charset=UTF-8")
	req.Header.Set("origin", "https://firefly.adobe.com")
	req.Header.Set("referer", "https://firefly.adobe.com/")
	req.Header.Set("user-agent", userAgent)
	req.Header.Set("cookie", cookie)
	resp, err := httpClientWithProxy(proxy).Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var payload struct {
		AccessToken string `json:"access_token"`
	}
	_ = json.Unmarshal(raw, &payload)
	return payload.AccessToken
}
