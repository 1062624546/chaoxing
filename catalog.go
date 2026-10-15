package main

import (
	"context"
	"encoding/json"
	"errors"
	"golang.org/x/net/html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

type Course struct{ ID, ClassID, CPI, Title, EntryURL string }
type Point struct {
	ID, Title        string
	Finished, Locked bool
	JobCount         int
	StudyEnc         string
	CatalogURL       string
}
type Job struct {
	Kind          string
	Raw, Property map[string]any
}

func (j Job) v(k string) string { return str(j.Raw[k]) }
func (j Job) p(k string) string { return str(j.Property[k]) }
func parseDOM(b []byte) (*html.Node, error) {
	n, e := html.Parse(strings.NewReader(string(b)))
	if e != nil {
		return nil, fail("format", "HTML 页面解析失败", 0)
	}
	return n, nil
}
func attr(n *html.Node, k string) string {
	if n == nil {
		return ""
	}
	for _, a := range n.Attr {
		if a.Key == k {
			return a.Val
		}
	}
	return ""
}
func hasClass(n *html.Node, k string) bool {
	for _, c := range strings.Fields(attr(n, "class")) {
		if c == k {
			return true
		}
	}
	return false
}
func collect(n *html.Node, pred func(*html.Node) bool) []*html.Node {
	out := []*html.Node{}
	var visit func(*html.Node)
	visit = func(x *html.Node) {
		if x == nil {
			return
		}
		if pred(x) {
			out = append(out, x)
		}
		for c := x.FirstChild; c != nil; c = c.NextSibling {
			visit(c)
		}
	}
	visit(n)
	return out
}
func first(n *html.Node, pred func(*html.Node) bool) *html.Node {
	ns := collect(n, pred)
	if len(ns) > 0 {
		return ns[0]
	}
	return nil
}
func class(n *html.Node, k string) *html.Node {
	return first(n, func(x *html.Node) bool { return hasClass(x, k) })
}
func tag(n *html.Node, k string) *html.Node {
	return first(n, func(x *html.Node) bool { return x.Type == html.ElementNode && x.Data == k })
}
func text(n *html.Node) string {
	if n == nil {
		return ""
	}
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(x *html.Node) {
		if x.Type == html.TextNode {
			b.WriteString(x.Data)
		}
		if x.Data == "script" || x.Data == "style" {
			return
		}
		for c := x.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return strings.TrimSpace(b.String())
}
func parseCourses(b []byte) ([]Course, error) {
	root, e := parseDOM(b)
	if e != nil {
		return nil, e
	}
	out := []Course{}
	for _, n := range collect(root, func(x *html.Node) bool { return x.Data == "div" && hasClass(x, "course") }) {
		if class(n, "not-open-tip") != nil {
			continue
		}
		c := Course{ID: attr(class(n, "courseId"), "value"), ClassID: attr(class(n, "clazzId"), "value"), Title: attr(class(n, "course-name"), "title")}
		if c.Title == "" {
			c.Title = text(class(n, "course-name"))
		}
		for _, a := range collect(n, func(x *html.Node) bool { return x.Data == "a" }) {
			u, e := url.Parse(attr(a, "href"))
			if e == nil && validCourseEntry(u, c) {
				c.CPI = u.Query().Get("cpi")
				c.EntryURL = attr(a, "href")
				break
			}
		}
		if c.ID == "" || c.ClassID == "" || c.CPI == "" {
			return nil, fail("format", "课程字段缺失，不能继续", 0)
		}
		out = append(out, c)
	}
	return out, nil
}

// Only accept course entry routes observed in the official course list.
func validCourseEntry(u *url.URL, c Course) bool {
	base, _ := url.Parse("https://mooc2-ans.chaoxing.com")
	u = base.ResolveReference(u)
	route := u.Hostname() == "mooc2-ans.chaoxing.com" && u.Path == "/mooc2-ans/mycourse/stu" || u.Hostname() == "mooc1.chaoxing.com" && u.Path == "/visit/stucoursemiddle"
	q := u.Query()
	return route && u.Scheme == "https" && u.User == nil && u.Port() == "" && q.Get("courseid") == c.ID && q.Get("clazzid") == c.ClassID && q.Get("cpi") != "" && (c.CPI == "" || q.Get("cpi") == c.CPI)
}

func (s *Session) courses(ctx context.Context) ([]Course, error) {
	address := "https://mooc2-ans.chaoxing.com/mooc2-ans/visit/courselistdata"
	form := url.Values{"courseType": {"1"}, "courseFolderId": {"0"}, "query": {""}, "superstarClass": {"0"}}
	b, e := s.postForm(ctx, address, form)
	if e != nil {
		return nil, e
	}
	courses, e := parseCourses(b)
	if e != nil {
		return nil, e
	}
	b, e = s.get(ctx, "https://mooc2-ans.chaoxing.com/mooc2-ans/visit/interaction", nil, nil, true)
	if e != nil {
		return nil, e
	}
	root, e := parseDOM(b)
	if e != nil {
		return nil, e
	}
	for _, n := range collect(root, func(x *html.Node) bool { return x.Data == "li" && attr(x, "fileid") != "" }) {
		form.Set("courseFolderId", attr(n, "fileid"))
		b, e = s.postForm(ctx, address, form)
		if e != nil {
			return nil, e
		}
		extra, e := parseCourses(b)
		if e != nil {
			return nil, e
		}
		courses = append(courses, extra...)
	}
	seen := map[string]bool{}
	out := []Course{}
	for _, c := range courses {
		key := c.ID + "/" + c.ClassID
		if !seen[key] {
			out = append(out, c)
			seen[key] = true
		}
	}
	return out, nil
}

var pointRE = regexp.MustCompile(`^cur([0-9]{1,20})$`)

func parsePoints(b []byte) ([]Point, error) {
	root, e := parseDOM(b)
	if e != nil {
		return nil, e
	}
	enc := attr(first(root, func(n *html.Node) bool { return n.Data == "input" && attr(n, "id") == "enc" }), "value")
	out := []Point{}
	seen := map[string]bool{}
	for _, n := range collect(root, func(x *html.Node) bool { return pointRE.MatchString(attr(x, "id")) }) {
		id := pointRE.FindStringSubmatch(attr(n, "id"))[1]
		if seen[id] {
			continue
		}
		seen[id] = true
		tip := text(class(n, "bntHoverTips"))
		p := Point{ID: id, StudyEnc: enc, Title: text(class(n, "clicktitle")), Finished: strings.Contains(tip, "已完成"), Locked: strings.Contains(tip, "解锁"), JobCount: int(number(attr(class(n, "knowledgeJobCount"), "value"), 1))}
		if p.Title == "" {
			return nil, fail("format", "章节名称缺失", 0)
		}
		out = append(out, p)
	}
	if len(out) == 0 && !strings.Contains(string(b), "暂无章节") {
		return nil, fail("format", "未识别到章节列表", 0)
	}
	return out, nil
}
func (s *Session) points(ctx context.Context, c Course) ([]Point, error) {
	base, _ := url.Parse("https://mooc2-ans.chaoxing.com")
	entry, err := url.Parse(c.EntryURL)
	if err != nil {
		return nil, fail("format", "课程入口链接无效", 0)
	}
	if c.EntryURL == "" {
		entry, _ = url.Parse("/mooc2-ans/mycourse/stu")
		entry.RawQuery = qv(map[string]string{"courseid": c.ID, "clazzid": c.ClassID, "cpi": c.CPI}).Encode()
	}
	entry = base.ResolveReference(entry)
	if !validCourseEntry(entry, c) {
		return nil, fail("format", "课程入口不是可信HTTPS地址", 0)
	}
	page, entryURL, err := s.getPage(ctx, entry.String(), nil, nil)
	if err != nil {
		return nil, err
	}
	q, err := catalogParams(page, c)
	if err != nil {
		return nil, err
	}
	address := "https://mooc2-ans.chaoxing.com/mooc2-ans/mycourse/studentcourse"
	b, catalogURL, err := s.getPage(ctx, address, q, map[string]string{"Referer": entryURL})
	if err != nil {
		return nil, err
	}
	points, err := parsePoints(b)
	if err != nil {
		return nil, err
	}
	for i := range points {
		points[i].CatalogURL = catalogURL
	}
	return points, nil
}
func catalogParams(page []byte, c Course) (url.Values, error) {
	root, err := parseDOM(page)
	if err != nil {
		return nil, err
	}
	fields := map[string]string{}
	for _, n := range collect(root, func(n *html.Node) bool { return n.Data == "input" }) {
		fields[attr(n, "id")] = attr(n, "value")
	}
	if fields["t"] == "" || fields["enc"] == "" {
		return nil, fail("format", "课程入口页缺少t或enc，停止使用不完整的章节目录请求", 0)
	}
	for key, want := range map[string]string{"courseid": c.ID, "clazzid": c.ClassID, "cpi": c.CPI} {
		if fields[key] != "" && fields[key] != want {
			return nil, fail("format", "课程入口页身份字段与所选课程不一致", 0)
		}
	}
	return qv(map[string]string{"courseid": c.ID, "clazzid": c.ClassID, "cpi": c.CPI, "ut": "s", "t": fields["t"], "stuenc": fields["enc"]}), nil
}

var mArgRE = regexp.MustCompile(`\bmArg\s*=\s*\{`)

func extractArg(b []byte) (map[string]any, error) {
	loc := mArgRE.FindIndex(b)
	if loc == nil {
		return nil, fail("format", "任务卡片格式无法识别", 0)
	}
	decoder := json.NewDecoder(strings.NewReader(string(b[loc[1]-1:])))
	var m map[string]any
	if decoder.Decode(&m) != nil {
		return nil, fail("format", "任务卡片 JSON 无法解析", 0)
	}
	return m, nil
}
func parseCards(b []byte) ([]Job, map[string]any, bool, error) {
	if strings.Contains(string(b), "章节未开放") {
		return nil, nil, false, fail("notopen", "章节未开放", 0)
	}
	if !mArgRE.Match(b) && strings.Contains(string(b), "暂无内容") {
		return nil, map[string]any{}, true, nil
	}
	arg, e := extractArg(b)
	if e != nil {
		return nil, nil, false, e
	}
	defaults := object(arg["defaults"])
	raw := arg["attachments"]
	if raw == nil {
		return []Job{}, defaults, false, nil
	}
	cards, ok := raw.([]any)
	if !ok {
		return nil, nil, false, fail("format", "任务附件字段无效", 0)
	}
	jobs := []Job{}
	for _, raw := range cards {
		card := object(raw)
		if len(card) == 0 {
			return nil, nil, false, fail("format", "任务附件无效", 0)
		}
		if truth(card["isPassed"]) {
			continue
		}
		prop := object(card["property"])
		kind := strings.ToLower(str(card["type"]))
		if card["job"] == nil {
			if kind != "read" || truth(prop["read"]) {
				continue
			}
		}
		if strings.Contains(kind, "live") || strings.Contains(str(prop["type"]), "live") || prop["liveId"] != nil || prop["streamName"] != nil || prop["vdoid"] != nil {
			kind = "live"
		}
		card["otherInfo"] = strings.SplitN(str(card["otherInfo"]), "&", 2)[0]
		jobs = append(jobs, Job{Kind: kind, Raw: card, Property: prop})
	}
	return jobs, defaults, false, nil
}
func (s *Session) jobs(ctx context.Context, c Course, p Point) ([]Job, map[string]any, error) {
	referer := ""
	if p.StudyEnc != "" {
		var err error
		referer, err = s.openChapter(ctx, c, p)
		if err != nil {
			return nil, nil, err
		}
	}
	headers := map[string]string{"Accept": "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8", "Accept-Language": "zh-CN,zh;q=0.9"}
	if referer != "" {
		headers["Referer"] = referer
	}
	jobs := []Job{}
	defaults := map[string]any{}
	seen := map[string]bool{}
	for num := 0; num < 7; num++ {
		if e := s.pace(ctx); e != nil {
			return nil, nil, e
		}
		q := qv(map[string]string{"clazzid": c.ClassID, "courseid": c.ID, "knowledgeid": p.ID, "ut": "s", "cpi": c.CPI, "v": "2025-0424-1038-3", "mooc2": "1", "isMicroCourse": "false", "editorPreview": "0", "num": str(num)})
		b, e := s.get(ctx, s.platform("/mooc-ans/knowledge/cards"), q, headers, true)
		if e != nil {
			return nil, nil, e
		}
		cards, d, empty, e := parseCards(b)
		if e != nil {
			return nil, nil, e
		}
		for k, v := range d {
			defaults[k] = v
		}
		for _, j := range cards {
			key := j.Kind + "/" + j.v("jobid") + "/" + j.v("objectId") + "/" + j.v("aid")
			if !seen[key] {
				jobs = append(jobs, j)
				seen[key] = true
			}
		}
		if empty {
			break
		}
	}
	if e := s.Jar.save(s.CookiePath); e != nil {
		return nil, nil, e
	}
	return jobs, defaults, nil
}
func selectCourses(cs []Course, ids []string) ([]Course, error) {
	if len(ids) == 0 {
		return nil, errors.New("请配置 course_list 或使用 --list-courses 查看课程")
	}
	wanted := map[string]bool{}
	found := map[string]bool{}
	for _, id := range ids {
		wanted[id] = true
	}
	out := []Course{}
	for _, c := range cs {
		if wanted[c.ID] {
			out = append(out, c)
			found[c.ID] = true
		}
	}
	for id := range wanted {
		if !found[id] {
			return nil, fail("config", "存在未匹配的课程 ID，请检查 course_list", 0)
		}
	}
	return out, nil
}

// Follow the official toOld() navigation; signatures originate from the course page.
func (s *Session) openChapter(ctx context.Context, c Course, p Point) (string, error) {
	target, err := url.Parse(s.platform("/mycourse/studentstudy"))
	if err != nil {
		return "", fail("config", "章节地址无效", 0)
	}
	target.RawQuery = qv(map[string]string{"chapterId": p.ID, "courseId": c.ID, "clazzid": c.ClassID, "cpi": c.CPI, "enc": p.StudyEnc, "mooc2": "1", "hidetype": "0", "fanyaVersion": "0"}).Encode()
	transfer, err := url.Parse(s.platform("/mycourse/transfer"))
	if err != nil {
		return "", err
	}
	transfer.RawQuery = qv(map[string]string{"moocId": c.ID, "clazzid": c.ClassID, "linkTime": str(time.Now().UnixMilli()), "ut": "s", "refer": target.String()}).Encode()
	finalURL := transfer.String()
	client := *s.Client
	policy := client.CheckRedirect
	client.CheckRedirect = func(r *http.Request, via []*http.Request) error {
		if policy != nil {
			if err := policy(r, via); err != nil {
				return err
			}
		}
		finalURL = r.URL.String()
		return nil
	}
	navigation := &Session{Client: &client, Jar: s.Jar, Base: s.Base, CookiePath: s.CookiePath}
	_, err = navigation.get(ctx, transfer.String(), nil, map[string]string{"Accept": "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8", "Accept-Language": "zh-CN,zh;q=0.9", "Referer": p.CatalogURL}, false)
	if err != nil {
		return "", err
	}
	final, err := url.Parse(finalURL)
	if err != nil || final.Hostname() != target.Hostname() || final.Path != "/mycourse/studentstudy" || final.Query().Get("chapterId") != p.ID || final.Query().Get("courseId") != c.ID || final.Query().Get("clazzid") != c.ClassID {
		return "", fail("format", "章节跳转未到达对应studentstudy页面，停止请求任务卡片", 0)
	}
	return finalURL, nil
}
