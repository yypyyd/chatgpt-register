package proxyutil

import (
	"bufio"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"
)

// AuthBridge 让 Chrome 连接一个本地无认证代理，桥接层再给每个 CONNECT / 普通
// 请求补上上游代理的认证凭据。Chromium 自身不支持在 --proxy-server 里带账号密码，
// 走桥接后同一任务的所有请求都会复用同一个上游会话（出口 IP 一致）。
type AuthBridge struct {
	listener net.Listener
	upstream *url.URL
	auth     string
	closed   chan struct{}
	once     sync.Once
}

// StartAuthBridge 在 127.0.0.1 上起一个桥接监听，返回桥与供 Chrome 使用的
// http://127.0.0.1:port 地址。仅支持 http/https 上游。
func StartAuthBridge(raw string) (*AuthBridge, string, error) {
	u, err := url.Parse(Normalize(raw))
	if err != nil || u.Hostname() == "" {
		return nil, "", fmt.Errorf("无效代理地址")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return nil, "", fmt.Errorf("认证代理桥仅支持 http/https 上游")
	}
	user, pass := "", ""
	if u.User != nil {
		user = u.User.Username()
		pass, _ = u.User.Password()
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, "", err
	}
	b := &AuthBridge{
		listener: ln,
		upstream: u,
		auth:     base64.StdEncoding.EncodeToString([]byte(user + ":" + pass)),
		closed:   make(chan struct{}),
	}
	go b.serve()
	return b, "http://" + ln.Addr().String(), nil
}

// Close 停止监听；可重复调用。
func (b *AuthBridge) Close() {
	b.once.Do(func() {
		close(b.closed)
		_ = b.listener.Close()
	})
}

func (b *AuthBridge) serve() {
	for {
		conn, err := b.listener.Accept()
		if err != nil {
			select {
			case <-b.closed:
				return
			default:
				continue
			}
		}
		go b.handle(conn)
	}
}

func (b *AuthBridge) handle(client net.Conn) {
	defer client.Close()
	clientReader := bufio.NewReader(client)
	head, err := readProxyHeaders(clientReader)
	if err != nil || len(head) == 0 {
		return
	}
	upstream, err := b.dialUpstream()
	if err != nil {
		return
	}
	defer upstream.Close()

	firstLine := string(head[:bytesLineEnd(head)])
	if strings.HasPrefix(strings.ToUpper(firstLine), "CONNECT ") {
		fields := strings.Fields(firstLine)
		if len(fields) < 2 {
			return
		}
		request := "CONNECT " + fields[1] + " HTTP/1.1\r\nHost: " + fields[1] +
			"\r\nProxy-Authorization: Basic " + b.auth + "\r\n\r\n"
		if _, err = io.WriteString(upstream, request); err != nil {
			return
		}
		upstreamReader := bufio.NewReader(upstream)
		response, err := readProxyHeaders(upstreamReader)
		if err != nil {
			return
		}
		if _, err = client.Write(response); err != nil || !proxyConnectOK(response) {
			return
		}
		relayProxy(client, clientReader, upstream, upstreamReader)
		return
	}

	head = injectProxyAuthorization(head, b.auth)
	if _, err = upstream.Write(head); err != nil {
		return
	}
	relayProxy(client, clientReader, upstream, bufio.NewReader(upstream))
}

func (b *AuthBridge) dialUpstream() (net.Conn, error) {
	host := b.upstream.Host
	if !strings.Contains(host, ":") {
		if strings.EqualFold(b.upstream.Scheme, "https") {
			host += ":443"
		} else {
			host += ":80"
		}
	}
	conn, err := net.DialTimeout("tcp", host, 20*time.Second)
	if err != nil {
		return nil, err
	}
	if strings.EqualFold(b.upstream.Scheme, "https") {
		tlsConn := tls.Client(conn, &tls.Config{ServerName: b.upstream.Hostname(), MinVersion: tls.VersionTLS12})
		if err = tlsConn.Handshake(); err != nil {
			_ = conn.Close()
			return nil, err
		}
		return tlsConn, nil
	}
	return conn, nil
}

func readProxyHeaders(r *bufio.Reader) ([]byte, error) {
	var out []byte
	for len(out) < 64*1024 {
		line, err := r.ReadBytes('\n')
		out = append(out, line...)
		if err != nil {
			return out, err
		}
		if string(line) == "\r\n" || string(line) == "\n" {
			return out, nil
		}
	}
	return nil, fmt.Errorf("代理请求头过大")
}

func bytesLineEnd(data []byte) int {
	for i, c := range data {
		if c == '\n' {
			return i
		}
	}
	return len(data)
}

func proxyConnectOK(response []byte) bool {
	line := string(response[:bytesLineEnd(response)])
	return strings.Contains(line, " 200 ")
}

func injectProxyAuthorization(head []byte, auth string) []byte {
	if strings.Contains(strings.ToLower(string(head)), "\r\nproxy-authorization:") {
		return head
	}
	marker := []byte("\r\n\r\n")
	idx := strings.Index(string(head), string(marker))
	if idx < 0 {
		return head
	}
	line := []byte("\r\nProxy-Authorization: Basic " + auth)
	out := make([]byte, 0, len(head)+len(line))
	out = append(out, head[:idx]...)
	out = append(out, line...)
	out = append(out, head[idx:]...)
	return out
}

func relayProxy(client net.Conn, clientReader *bufio.Reader, upstream net.Conn, upstreamReader *bufio.Reader) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upstream, clientReader); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, upstreamReader); done <- struct{}{} }()
	<-done
}
