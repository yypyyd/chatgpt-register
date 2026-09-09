package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	http "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

// replayOpenAI 离线重放浏览器抓到的注册请求序列。
// 只重放对注册有决定性作用的 POST（authorize / create-account / verify 等），
// GET 静态资源和轮询跳过。目的是看服务端是否接受非浏览器客户端的同样载荷。
func replayOpenAI(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	// 收集所有 request 记录（按时间顺序）
	var reqs []rec
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		var r rec
		if json.Unmarshal(sc.Bytes(), &r) == nil && r.Kind == "request" {
			reqs = append(reqs, r)
		}
	}
	fmt.Printf("[replay] 共 %d 条请求\n", len(reqs))

	// 建 Chrome 指纹 HTTP 客户端
	jar := tls_client.NewCookieJar()
	cli, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(),
		tls_client.WithClientProfile(profiles.Chrome_131),
		tls_client.WithRandomTLSExtensionOrder(),
		tls_client.WithCookieJar(jar),
		tls_client.WithNotFollowRedirects(),
	)
	if err != nil {
		return err
	}
	if p := os.Getenv("PROXY_URL"); p != "" {
		// 重放时是否走代理由环境决定
		_ = p
	}

	// 只挑 auth.openai.com 的 POST 重放
	var posts []rec
	for _, r := range reqs {
		if strings.Contains(r.URL, "auth.openai.com") && strings.EqualFold(r.Method, "POST") {
			posts = append(posts, r)
		}
	}
	fmt.Printf("[replay] auth.openai.com POST 共 %d 条\n", len(posts))
	for i, r := range posts {
		fmt.Printf("\n=== [%d] %s %s ===\n", i, r.Method, r.URL)
		fmt.Printf("req body: %s\n", trunc(r.Body, 400))
		resp, err := doReq(cli, r)
		if err != nil {
			fmt.Printf("!! 请求失败: %v\n", err)
			continue
		}
		fmt.Printf("resp %d\n%s\n", resp.StatusCode, trunc(readBody(resp), 600))
		resp.Body.Close()
	}
	return nil
}

func doReq(cli tls_client.HttpClient, r rec) (*http.Response, error) {
	var body io.Reader
	if r.Body != "" {
		body = strings.NewReader(r.Body)
	}
	req, err := http.NewRequest(strings.ToUpper(r.Method), r.URL, body)
	if err != nil {
		return nil, err
	}
	// 复制浏览器请求头（去掉伪头/连接管理头）
	skip := map[string]bool{
		"host": true, "content-length": true, "connection": true,
		"accept-encoding": true, ":method": true, ":path": true, ":authority": true, ":scheme": true,
	}
	for k, v := range r.Headers {
		lk := strings.ToLower(k)
		if skip[lk] {
			continue
		}
		req.Header.Set(k, v)
	}
	return cli.Do(req)
}

func readBody(resp *http.Response) string {
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}
