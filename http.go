package main

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type Failure struct {
	Kind    string
	Status  int
	Message string
}

func (e *Failure) Error() string { return e.Message }
func retryable(e error) bool {
	var f *Failure
	return errors.As(e, &f) && (f.Kind == "network" || f.Kind == "server")
}
func fail(kind, msg string, status int) error {
	return &Failure{Kind: kind, Message: msg, Status: status}
}

type Session struct {
	Client     *http.Client
	Jar        *persistentJar
	Base       string
	CookiePath string
	rateMu     sync.Mutex
	last       time.Time
}

func newSession(j *persistentJar, path string) *Session {
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 20 * time.Second, MaxIdleConns: 16, MaxIdleConnsPerHost: 4, IdleConnTimeout: 60 * time.Second}
	client := &http.Client{Transport: transport, Jar: j, Timeout: 30 * time.Second, CheckRedirect: func(r *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return fail("redirect", "登录或页面重定向超过5次，请使用有效 Cookie", 0)
		}
		if !validDomain(r.URL.Hostname()) {
			return fail("redirect", "平台重定向至非学习通域名，已停止", 0)
		}
		return nil
	}}
	return &Session{Client: client, Jar: j, Base: "https://mooc1.chaoxing.com", CookiePath: path}
}
func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (s *Session) pace(ctx context.Context) error {
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	if e := sleepContext(ctx, time.Until(s.last.Add(500*time.Millisecond))); e != nil {
		return e
	}
	s.last = time.Now()
	return nil
}
func (s *Session) platform(path string) string { return strings.TrimRight(s.Base, "/") + path }
func (s *Session) request(ctx context.Context, method, address string, query url.Values, body []byte, headers map[string]string, readOnly bool) ([]byte, error) {
	attempts := 1
	if readOnly && (method == "GET" || method == "HEAD") {
		attempts = 3
	}
	for attempt := 0; attempt < attempts; attempt++ {
		if e := ctx.Err(); e != nil {
			return nil, e
		}
		u, e := url.Parse(address)
		if e != nil || u.Host == "" || u.Scheme != "https" && u.Scheme != "http" {
			return nil, fail("config", "请求地址格式错误", 0)
		}
		if len(query) > 0 {
			q := u.Query()
			for k, vs := range query {
				q[k] = vs
			}
			u.RawQuery = q.Encode()
		}
		req, e := http.NewRequestWithContext(ctx, method, u.String(), strings.NewReader(string(body)))
		if e != nil {
			return nil, fail("config", "请求构造失败", 0)
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/118.0.0.0 Safari/537.36")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, e := s.Client.Do(req)
		if e != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			var f *Failure
			if errors.As(e, &f) {
				return nil, f
			}
			if attempt+1 < attempts {
				if e = sleepContext(ctx, time.Duration(1<<attempt)*500*time.Millisecond); e != nil {
					return nil, e
				}
				continue
			}
			return nil, fail("network", "网络请求失败或超时（请求详情已隐藏）", 0)
		}
		data, e := io.ReadAll(io.LimitReader(resp.Body, (16<<20)+1))
		resp.Body.Close()
		if e != nil {
			return nil, fail("network", "读取响应失败，不重放当前请求", 0)
		}
		if len(data) > 16<<20 {
			return nil, fail("format", "响应超过16MB限制", 0)
		}
		if resp.StatusCode == 403 {
			return nil, fail("blocked", "HTTP 403：平台拒绝访问，停止重试", 403)
		}
		if resp.StatusCode == 401 || resp.Request.URL.Hostname() == "passport2.chaoxing.com" && resp.Request.URL.Path != "/fanyalogin" || strings.Contains(string(data), `id="loginBtn"`) || strings.Contains(string(data), `id="formLogin"`) {
			return nil, fail("expired", "登录会话失效，请更新 Cookie", resp.StatusCode)
		}
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			return nil, fail("blocked", fmt.Sprintf("HTTP %d：请求被拒绝，停止重试", resp.StatusCode), resp.StatusCode)
		}
		if resp.StatusCode != 200 {
			return nil, fail("server", fmt.Sprintf("HTTP %d：请求未成功", resp.StatusCode), resp.StatusCode)
		}
		return data, nil
	}
	return nil, fail("network", "请求尝试次数耗尽", 0)
}
func (s *Session) get(ctx context.Context, address string, q url.Values, headers map[string]string, readOnly bool) ([]byte, error) {
	return s.request(ctx, "GET", address, q, nil, headers, readOnly)
}
func (s *Session) postForm(ctx context.Context, address string, form url.Values) ([]byte, error) {
	return s.request(ctx, "POST", address, nil, []byte(form.Encode()), map[string]string{"Content-Type": "application/x-www-form-urlencoded; charset=UTF-8", "X-Requested-With": "XMLHttpRequest"}, false)
}
func (s *Session) getJSON(ctx context.Context, address string, q url.Values, headers map[string]string, target any) error {
	b, e := s.get(ctx, address, q, headers, true)
	if e != nil {
		return e
	}
	if json.Unmarshal(b, target) != nil {
		return fail("format", "接口返回非预期 JSON", 0)
	}
	return nil
}
func encryptCredential(text string) string {
	key := []byte("u2oh6Vu^HWe4_AES")
	data := []byte(text)
	n := aes.BlockSize - len(data)%aes.BlockSize
	data = append(data, []byte(strings.Repeat(string(byte(n)), n))...)
	block, _ := aes.NewCipher(key)
	out := make([]byte, len(data))
	cipher.NewCBCEncrypter(block, key).CryptBlocks(out, data)
	return base64.StdEncoding.EncodeToString(out)
}
func (s *Session) login(ctx context.Context, c Config) error {
	if !yes(c.Common["use_cookies"]) {
		if c.Common["username"] == "" || c.Common["password"] == "" {
			return fail("config", "配置缺少账号密码或 use_cookies=true", 0)
		}
		clone := *s.Client
		clone.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		original := s.Client
		s.Client = &clone
		b, e := s.postForm(ctx, "https://passport2.chaoxing.com/fanyalogin", url.Values{"fid": {"-1"}, "uname": {encryptCredential(c.Common["username"])}, "password": {encryptCredential(c.Common["password"])}, "refer": {"https%3A%2F%2Fi.chaoxing.com"}, "t": {"true"}, "forbidotherlogin": {"0"}, "validate": {""}, "doubleFactorLogin": {"0"}, "independentId": {"0"}})
		s.Client = original
		if e != nil {
			var f *Failure
			if errors.As(e, &f) && f.Status >= 300 && f.Status < 400 {
				return fail("redirect", "账号密码登录返回重定向，请使用浏览器登录后导入 Cookie", f.Status)
			}
			return e
		}
		var result map[string]any
		if json.Unmarshal(b, &result) != nil {
			return fail("format", "登录返回非 JSON 页面，请使用 Cookie 登录", 0)
		}
		if !truth(result["status"]) {
			return fail("expired", "账号密码登录失败，请在官方页面检查并使用 Cookie", 0)
		}
	}
	uid, e := s.Jar.value("_uid")
	if e != nil {
		return e
	}
	if uid == "" {
		uid, e = s.Jar.value("UID")
	}
	if e != nil {
		return e
	}
	if uid == "" {
		return fail("expired", "缺少有效用户身份 Cookie", 0)
	}
	_, e = s.postForm(ctx, "https://mooc2-ans.chaoxing.com/mooc2-ans/visit/courselistdata", url.Values{"courseType": {"1"}, "courseFolderId": {"0"}, "query": {""}, "superstarClass": {"0"}})
	if e != nil {
		return e
	}
	if e = s.Jar.save(s.CookiePath); e != nil {
		return e
	}
	info("登录会话验证成功")
	return nil
}
func truth(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case float64:
		return x != 0
	case string:
		return yes(x)
	}
	return false
}
func str(v any) string {
	if v == nil {
		return ""
	}
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return fmt.Sprintf("%g", x)
	case json.Number:
		return string(x)
	default:
		return fmt.Sprint(v)
	}
}
func object(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}
func qv(m map[string]string) url.Values {
	q := url.Values{}
	for k, v := range m {
		q.Set(k, v)
	}
	return q
}
