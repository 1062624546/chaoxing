package main

import (
	"encoding/json"
	"errors"
	"golang.org/x/net/publicsuffix"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type storedCookie struct {
	Name            string         `json:"name"`
	Value           string         `json:"value"`
	Domain          string         `json:"domain"`
	Path            string         `json:"path"`
	Secure          bool           `json:"secure"`
	Expires         *int64         `json:"expires"`
	DomainSpecified *bool          `json:"domain_specified,omitempty"`
	HTTPOnly        bool           `json:"httponly,omitempty"`
	Rest            map[string]any `json:"_rest,omitempty"`
}
type persistentJar struct {
	mu      sync.Mutex
	jar     *cookiejar.Jar
	records map[string]storedCookie
}

func validDomain(s string) bool {
	s = strings.TrimPrefix(strings.ToLower(s), ".")
	return s == "chaoxing.com" || strings.HasSuffix(s, ".chaoxing.com")
}
func cookieKey(c storedCookie) string {
	return strings.TrimPrefix(c.Domain, ".") + "\x00" + c.Path + "\x00" + c.Name
}
func newJar() *persistentJar {
	j, _ := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	return &persistentJar{jar: j, records: map[string]storedCookie{}}
}
func (j *persistentJar) Cookies(u *url.URL) []*http.Cookie { return j.jar.Cookies(u) }
func (j *persistentJar) SetCookies(u *url.URL, cs []*http.Cookie) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.jar.SetCookies(u, cs)
	if !validDomain(u.Hostname()) {
		return
	}
	for _, c := range cs {
		d := c.Domain
		specified := d != ""
		if d == "" {
			d = u.Hostname()
		}
		if !validDomain(d) {
			continue
		}
		path := c.Path
		if path == "" {
			path = defaultCookiePath(u.Path)
		}
		r := storedCookie{Name: c.Name, Value: c.Value, Domain: d, Path: path, Secure: c.Secure, HTTPOnly: c.HttpOnly, DomainSpecified: &specified}
		if !c.Expires.IsZero() {
			v := c.Expires.Unix()
			r.Expires = &v
		}
		if c.MaxAge > 0 {
			v := time.Now().Add(time.Duration(c.MaxAge) * time.Second).Unix()
			r.Expires = &v
		}
		key := cookieKey(r)
		if c.MaxAge < 0 || (r.Expires != nil && *r.Expires <= time.Now().Unix()) {
			delete(j.records, key)
		} else {
			j.records[key] = r
		}
	}
}
func defaultCookiePath(p string) string {
	if !strings.HasPrefix(p, "/") {
		return "/"
	}
	n := strings.LastIndex(p, "/")
	if n <= 0 {
		return "/"
	}
	return p[:n]
}
func (j *persistentJar) add(r storedCookie) error {
	// Cookie header values may use surrounding double quotes (RFC 6265).
	if len(r.Value) >= 2 && r.Value[0] == '"' && r.Value[len(r.Value)-1] == '"' {
		r.Value = r.Value[1 : len(r.Value)-1]
	}
	if e := (&http.Cookie{Name: r.Name, Value: r.Value}).Valid(); e != nil {
		return errors.New("Cookie 名称或值包含非法字符，请重新导出 Cookie（内容已隐藏）")
	}

	if !validDomain(r.Domain) || r.Name == "" {
		return errors.New("Cookie 域名或名称无效")
	}
	if r.Expires != nil && *r.Expires <= time.Now().Unix() {
		return nil
	}
	if r.Path == "" {
		r.Path = "/"
	}
	u := &url.URL{Scheme: "https", Host: strings.TrimPrefix(r.Domain, "."), Path: r.Path}
	c := &http.Cookie{Name: r.Name, Value: r.Value, Domain: r.Domain, Path: r.Path, Secure: r.Secure, HttpOnly: r.HTTPOnly}
	if r.Rest != nil {
		_, c.HttpOnly = r.Rest["HttpOnly"]
	}
	if r.Expires != nil {
		c.Expires = time.Unix(*r.Expires, 0)
	}
	if r.DomainSpecified != nil && !*r.DomainSpecified {
		c.Domain = ""
	}
	j.jar.SetCookies(u, []*http.Cookie{c})
	j.records[cookieKey(r)] = r
	return nil
}
func loadCookies(path string) (*persistentJar, error) {
	j := newJar()
	b, e := os.ReadFile(path)
	if e != nil {
		return nil, errors.New("Cookie 文件无法读取，请在本地导入有效 Cookie")
	}
	raw := strings.TrimSpace(strings.TrimPrefix(string(b), "\ufeff"))
	if raw == "" {
		return nil, errors.New("Cookie 文件为空")
	}
	if strings.HasPrefix(raw, "[") {
		var records []storedCookie
		if json.Unmarshal([]byte(raw), &records) != nil {
			return nil, errors.New("Cookie JSON 格式无效")
		}
		for _, r := range records {
			if e := j.add(r); e != nil {
				return nil, e
			}
		}
	} else {
		if strings.HasPrefix(strings.ToLower(raw), "cookie:") {
			raw = strings.TrimSpace(raw[7:])
		}
		for _, part := range strings.Split(raw, ";") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			name, value, ok := strings.Cut(part, "=")
			if !ok || strings.ContainsAny(name, " \t\r\n") {
				return nil, errors.New("Cookie 文本格式无效")
			}
			if e := j.add(storedCookie{Name: name, Value: value, Domain: ".chaoxing.com", Path: "/"}); e != nil {
				return nil, e
			}
		}
	}
	return j, nil
}
func atomicWrite(path string, b []byte) error {
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return errors.New("持久化目录无法创建")
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".chaoxing-*")
	if e != nil {
		return errors.New("持久化临时文件无法创建")
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, e = f.Write(b); e != nil {
		f.Close()
		return errors.New("持久化写入失败")
	}
	if e = f.Sync(); e != nil {
		f.Close()
		return errors.New("持久化同步失败")
	}
	if e = f.Close(); e != nil {
		return errors.New("持久化关闭失败")
	}
	if e = os.Rename(tmp, path); e != nil {
		return errors.New("持久化原子替换失败")
	}
	return nil
}
func (j *persistentJar) save(path string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	rs := []storedCookie{}
	for _, r := range j.records {
		if r.Expires == nil || *r.Expires > time.Now().Unix() {
			rs = append(rs, r)
		}
	}
	b, e := json.Marshal(rs)
	if e != nil {
		return errors.New("Cookie 编码失败")
	}
	return atomicWrite(path, b)
}
func (j *persistentJar) value(name string) (string, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	value := ""
	for _, r := range j.records {
		if r.Name == name && (r.Expires == nil || *r.Expires > time.Now().Unix()) {
			if value != "" && value != r.Value {
				return "", errors.New("Cookie 身份冲突，请重新导入")
			}
			value = r.Value
		}
	}
	return value, nil
}
