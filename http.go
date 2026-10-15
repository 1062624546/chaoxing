package main

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"golang.org/x/net/html"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
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
			return fail("redirect", fmt.Sprintf("页面重定向超过5次：%s → %s（参数已隐藏）", via[len(via)-1].URL.Hostname()+via[len(via)-1].URL.EscapedPath(), r.URL.Hostname()+r.URL.EscapedPath()), 0)
		}
		// The captured personal-space redirect advertises HTTP, while Chrome
		// follows the identical URL over HTTPS. Never send this hop over HTTP.
		if len(via) > 0 && r.URL.Scheme == "http" && r.URL.Host == "i.chaoxing.com" && r.URL.User == nil && r.URL.Path == "/base" {
			previous := via[len(via)-1].URL
			if previous.Scheme == "https" && previous.Host == r.URL.Host && previous.Path == r.URL.Path {
				r.URL.Scheme = "https"
			}
		}
		if r.URL.Scheme != "https" || !validDomain(r.URL.Hostname()) {
			return fail("redirect", "平台重定向至非学习通域名或非HTTPS地址，已停止", 0)
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
	return s.requestURL(ctx, method, address, query, body, headers, readOnly, nil)
}

func (s *Session) requestURL(ctx context.Context, method, address string, query url.Values, body []byte, headers map[string]string, readOnly bool, finalURL *string) ([]byte, error) {
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
			if k != "X-Login-Page" {
				req.Header.Set(k, v)
			}
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
			info("403诊断：响应类型=%s；响应字节数=%d；Referer=%s；页面提示=%s", resp.Header.Get("Content-Type"), len(data), debugReferer(resp.Request.Header.Get("Referer")), responseHint(data))
			return nil, fail("blocked", fmt.Sprintf("HTTP 403：%s %s%s 被拒绝，停止重试（参数已隐藏）", method, resp.Request.URL.Hostname(), resp.Request.URL.EscapedPath()+" 参数="+debugQuery(resp.Request.URL.Query())), 403)
		}
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			target, err := resp.Location()
			destination := "未提供有效跳转地址"
			if err == nil {
				destination = target.Hostname() + target.EscapedPath()
			}
			return nil, fail("redirect", fmt.Sprintf("HTTP %d：%s 请求重定向至 %s（参数已隐藏）", resp.StatusCode, method, destination), resp.StatusCode)
		}
		if resp.StatusCode == 401 || headers["X-Login-Page"] != "1" && (resp.Request.URL.Hostname() == "passport2.chaoxing.com" && resp.Request.URL.Path != "/fanyalogin" || strings.Contains(string(data), `id="loginBtn"`) || strings.Contains(string(data), `id="formLogin"`)) {
			return nil, fail("expired", "登录会话失效，请更新 Cookie", resp.StatusCode)
		}
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			return nil, fail("blocked", fmt.Sprintf("HTTP %d：请求被拒绝，停止重试", resp.StatusCode), resp.StatusCode)
		}
		if resp.StatusCode != 200 {
			return nil, fail("server", fmt.Sprintf("HTTP %d：请求未成功", resp.StatusCode), resp.StatusCode)
		}
		if finalURL != nil {
			*finalURL = resp.Request.URL.String()
		}
		return data, nil
	}
	return nil, fail("network", "请求尝试次数耗尽", 0)
}
func (s *Session) get(ctx context.Context, address string, q url.Values, headers map[string]string, readOnly bool) ([]byte, error) {
	return s.request(ctx, "GET", address, q, nil, headers, readOnly)
}

// Use the URL of the successful response, including all platform redirects.
func (s *Session) getPage(ctx context.Context, address string, q url.Values, headers map[string]string) ([]byte, string, error) {
	var finalURL string
	b, err := s.requestURL(ctx, "GET", address, q, nil, headers, true, &finalURL)
	return b, finalURL, err
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
		info("账号密码登录阶段：打开官方登录页")
		pageURL := "https://passport2.chaoxing.com/login"
		var finalPageURL string
		page, e := s.requestURL(ctx, "GET", pageURL, url.Values{"fid": {""}, "newversion": {"true"}, "refer": {"https://i.chaoxing.com"}}, nil, map[string]string{"X-Login-Page": "1"}, false, &finalPageURL)
		if e != nil {
			return e
		}
		form, e := passwordForm(page, c.Common["username"], c.Common["password"])
		if e != nil {
			return e
		}
		clone := *s.Client
		clone.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		authSession := &Session{Client: &clone, Jar: s.Jar, Base: s.Base, CookiePath: s.CookiePath}
		info("账号密码登录阶段：提交官方登录表单")
		b, e := authSession.request(ctx, "POST", "https://passport2.chaoxing.com/fanyalogin", nil, []byte(form.Encode()), map[string]string{
			"Content-Type": "application/x-www-form-urlencoded; charset=UTF-8", "X-Requested-With": "XMLHttpRequest", "Origin": "https://passport2.chaoxing.com", "Referer": finalPageURL,
		}, false)
		if e != nil {
			return e
		}
		var result map[string]any
		if json.Unmarshal(b, &result) != nil {
			return fail("format", "登录返回非 JSON 页面，请使用 Cookie 登录", 0)
		}
		if truth(result["containTwoFactorLogin"]) || truth(result["weakpwd"]) {
			return fail("verification", "平台要求双因子验证或修改弱密码，请在官方网页完成", 0)
		}
		if !truth(result["status"]) {
			message := str(result["msg2"]) + str(result["msg"]) + str(result["mes"])
			switch {
			case strings.Contains(message, "验证码"), strings.Contains(message, "验证"), strings.Contains(message, "频繁"):
				return fail("verification", "登录接口要求验证或限制登录频率，请在官方网页确认", 0)
			case strings.Contains(message, "密码"), strings.Contains(message, "账号"), strings.Contains(message, "用户名"):
				return fail("credentials", "登录接口报告账号或密码错误，请核对配置", 0)
			default:
				return fail("credentials", "登录接口拒绝账号密码，返回 status=false（响应内容已隐藏）", 0)
			}
		}
		if destination := str(result["url"]); destination != "" {
			decoded, err := url.PathUnescape(destination)
			if err != nil {
				return fail("format", "登录返回的跳转地址无法解析", 0)
			}
			target, err := url.Parse(decoded)
			if err != nil || target.Scheme != "https" || !validDomain(target.Hostname()) || target.User != nil {
				return fail("redirect", "登录返回非可信HTTPS跳转地址，已停止", 0)
			}
			info("账号密码登录阶段：访问平台返回的登录跳转页")
			if _, err = s.get(ctx, target.String(), nil, nil, false); err != nil {
				return err
			}
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

// Read the same hidden form fields used by the official password login page.
func passwordForm(page []byte, username, password string) (url.Values, error) {
	root, e := parseDOM(page)
	if e != nil {
		return nil, e
	}
	inputs := collect(root, func(n *html.Node) bool { return n.Type == html.ElementNode && n.Data == "input" })
	fields := map[string]string{}
	for _, n := range inputs {
		fields[attr(n, "id")] = attr(n, "value")
	}
	if fields["needVcode"] == "1" {
		return nil, fail("verification", "平台要求验证码，请在官方网页完成验证", 0)
	}
	if fields["t"] != "true" || fields["fid"] == "" || fields["refer"] == "" {
		return nil, fail("format", "官方登录页面缺少预期表单字段，停止提交账号密码", 0)
	}
	form := url.Values{}
	for _, key := range []string{"fid", "refer", "t", "forbidotherlogin", "validate", "doubleFactorLogin", "independentId", "independentNameId"} {
		form.Set(key, fields[key])
	}
	form.Set("uname", encryptCredential(strings.TrimSpace(username)))
	form.Set("password", encryptCredential(password))
	return form, nil
}

// Only known non-secret routing fields are printable; unknown fields fail closed.
func debugQuery(q url.Values) string {
	safe := map[string]bool{"ut": true, "mooc2": true, "courseid": true, "courseId": true, "clazzid": true, "clazzId": true, "knowledgeid": true, "chapterId": true, "cpi": true, "num": true, "v": true, "isMicroCourse": true, "editorPreview": true, "courseType": true, "courseFolderId": true, "superstarClass": true}
	out := url.Values{}
	for key, values := range q {
		if safe[key] {
			out[key] = values
		} else {
			out.Set(key, "[隐藏]")
		}
	}
	return out.Encode()
}

func debugReferer(address string) string {
	if address == "" {
		return "未发送"
	}
	u, e := url.Parse(address)
	if e != nil || u.Hostname() == "" {
		return "无效地址"
	}
	return u.Hostname() + u.EscapedPath() + "?" + debugQuery(u.Query())
}

var opaqueDiagnosticRE = regexp.MustCompile(`[A-Za-z0-9_+/%.=-]{24,}`)
var personalDiagnosticRE = regexp.MustCompile(`\b[0-9]{11,}\b|[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)

func responseHint(data []byte) string {
	root, e := parseDOM(data)
	if e != nil {
		return "响应无法解析"
	}
	// Never log scripts, style, hidden fields, links, or attribute values.
	var remove func(*html.Node)
	remove = func(n *html.Node) {
		for c := n.FirstChild; c != nil; {
			next := c.NextSibling
			if c.Data == "script" || c.Data == "style" || c.Data == "input" || c.Data == "textarea" || attr(c, "hidden") != "" || strings.Contains(strings.ReplaceAll(attr(c, "style"), " ", ""), "display:none") {
				n.RemoveChild(c)
			} else {
				remove(c)
			}
			c = next
		}
	}
	remove(root)
	hint := strings.Join(strings.Fields(text(root)), " ")
	hint = redact(hint)
	hint = opaqueDiagnosticRE.ReplaceAllString(hint, "[长值已隐藏]")
	hint = personalDiagnosticRE.ReplaceAllString(hint, "[个人信息已隐藏]")
	chars := []rune(hint)
	if len(chars) > 500 {
		hint = string(chars[:500]) + "…"
	}
	if hint == "" {
		return "无可见错误文本"
	}
	return hint
}
