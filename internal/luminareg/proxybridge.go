package luminareg

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
	"sync/atomic"
	"time"
)

// localAuthProxyBridge 让 Chrome 连接一个本地无认证代理，桥接层再给每个 CONNECT
// 请求补上上游认证凭据（上游无认证时仅转发），并统计经代理的上下行字节数。
// 与 grokreg/adobereg 中同名实现同源，仅供 Lumina 模块独立使用。
type localAuthProxyBridge struct {
	listener net.Listener
	upstream *url.URL
	auth     string
	closed   chan struct{}
	once     sync.Once

	up, down atomic.Int64
}

func startLocalAuthProxyBridge(raw string) (*localAuthProxyBridge, string, error) {
	u, err := url.Parse(normalizeProxy(raw))
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
	auth := ""
	if user != "" || pass != "" {
		auth = base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
	}
	b := &localAuthProxyBridge{
		listener: ln,
		upstream: u,
		auth:     auth,
		closed:   make(chan struct{}),
	}
	go b.serve()
	return b, "http://" + ln.Addr().String(), nil
}

// Traffic 返回经代理转发的字节数（上行发往上游 / 下行回给浏览器）。
func (b *localAuthProxyBridge) Traffic() (up, down int64) {
	return b.up.Load(), b.down.Load()
}

func (b *localAuthProxyBridge) Close() {
	b.once.Do(func() {
		close(b.closed)
		_ = b.listener.Close()
	})
}

func (b *localAuthProxyBridge) serve() {
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

func (b *localAuthProxyBridge) handle(client net.Conn) {
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
		request := "CONNECT " + fields[1] + " HTTP/1.1\r\nHost: " + fields[1] + "\r\n"
		if b.auth != "" {
			request += "Proxy-Authorization: Basic " + b.auth + "\r\n"
		}
		request += "\r\n"
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
		b.relayProxy(client, clientReader, upstream, upstreamReader)
		return
	}

	if b.auth != "" {
		head = injectProxyAuthorization(head, b.auth)
	}
	b.up.Add(int64(len(head)))
	if _, err = upstream.Write(head); err != nil {
		return
	}
	b.relayProxy(client, clientReader, upstream, bufio.NewReader(upstream))
}

func (b *localAuthProxyBridge) dialUpstream() (net.Conn, error) {
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

func (b *localAuthProxyBridge) relayProxy(client net.Conn, clientReader *bufio.Reader, upstream net.Conn, upstreamReader *bufio.Reader) {
	done := make(chan struct{}, 2)
	go func() {
		n, _ := io.Copy(upstream, clientReader)
		b.up.Add(n)
		done <- struct{}{}
	}()
	go func() {
		n, _ := io.Copy(client, upstreamReader)
		b.down.Add(n)
		done <- struct{}{}
	}()
	<-done
}
