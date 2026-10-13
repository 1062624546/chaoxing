package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func fixtureSession(t *testing.T, handler http.HandlerFunc) (*Session, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	s := newSession(newJar(), filepath.Join(t.TempDir(), "cookies.txt"))
	s.Base = server.URL
	return s, server
}
func TestParseINIPreservesCredentials(t *testing.T) {
	sections, e := parseINI("\ufeff[common]\npassword=abc%xyz=a;b#c\nuse_cookies=true\n[tiku]\nkey=secret\n")
	if e != nil || sections["common"]["password"] != "abc%xyz=a;b#c" {
		t.Fatal("INI value altered", e)
	}
}
func TestINIRejectsDuplicates(t *testing.T) {
	for _, input := range []string{"[common]\njobs=1\njobs=2", "jobs=1", "[common]\ninvalid"} {
		if _, e := parseINI(input); e == nil {
			t.Fatal("expected invalid INI")
		}
	}
}
func TestConfigDefaultsAndValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.ini")
	os.WriteFile(path, []byte("[common]\nuse_cookies=true\n[tiku]\n"), 0600)
	c, e := loadConfig(path)
	if e != nil || c.Jobs != 4 || c.Speed != 1 || c.CookiePath != filepath.Join(filepath.Dir(path), "cookies.txt") {
		t.Fatal(c, e)
	}
	for _, setting := range []string{"jobs=0", "speed=3", "use_cookies=perhaps", "notopen_action=unknown"} {
		os.WriteFile(path, []byte("[common]\n"+setting), 0600)
		if _, e = loadConfig(path); e == nil {
			t.Fatal("invalid config accepted", setting)
		}
	}
}
func TestCookieTextImport(t *testing.T) {
	p := filepath.Join(t.TempDir(), "cookies.txt")
	os.WriteFile(p, []byte("\ufeffCookie: _uid=123; token=a=b;"), 0600)
	j, e := loadCookies(p)
	if e != nil {
		t.Fatal(e)
	}
	u, _ := url.Parse("https://mooc1.chaoxing.com/mooc-ans/knowledge/cards")
	cs := j.Cookies(u)
	if len(cs) != 2 {
		t.Fatal(cs)
	}
	other, _ := url.Parse("https://example.com/")
	if len(j.Cookies(other)) != 0 {
		t.Fatal("cookies leaked to unrelated host")
	}
	v, _ := j.value("token")
	if v != "a=b" {
		t.Fatal("equals sign lost")
	}
}
func TestCookiePythonJSONRoundtrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "cookies.txt")
	data := `[{"name":"_uid","value":"123","domain":".chaoxing.com","path":"/","secure":true,"expires":null,"domain_specified":true,"_rest":{"HttpOnly":null}},{"name":"x","value":"host","domain":"mooc1.chaoxing.com","path":"/cards","secure":false,"expires":null,"domain_specified":false},{"name":"old","value":"expired","domain":".chaoxing.com","path":"/","expires":1}]`
	os.WriteFile(p, []byte(data), 0600)
	j, e := loadCookies(p)
	if e != nil {
		t.Fatal(e)
	}
	if e = j.save(p); e != nil {
		t.Fatal(e)
	}
	j, e = loadCookies(p)
	if e != nil {
		t.Fatal(e)
	}
	u, _ := url.Parse("https://other.mooc1.chaoxing.com/cards")
	for _, c := range j.Cookies(u) {
		if c.Name == "x" {
			t.Fatal("host-only scope expanded")
		}
	}
	u, _ = url.Parse("https://mooc1.chaoxing.com/cards")
	if len(j.Cookies(u)) != 2 {
		t.Fatal("domain/path roundtrip")
	}
	if osInfo, e := os.Stat(p); e != nil || osInfo.Mode().Perm() != 0600 {
		t.Fatal("cookie permissions")
	}
}
func TestCookieExpiryAndConflict(t *testing.T) {
	j := newJar()
	j.add(storedCookie{Name: "_uid", Value: "123", Domain: ".chaoxing.com", Path: "/"})
	j.add(storedCookie{Name: "_uid", Value: "123", Domain: "mooc1.chaoxing.com", Path: "/"})
	if _, e := j.value("_uid"); e != nil {
		t.Fatal(e)
	}
	j.add(storedCookie{Name: "_uid", Value: "456", Domain: "mooc1.chaoxing.com", Path: "/"})
	if _, e := j.value("_uid"); e == nil {
		t.Fatal("identity conflict ignored")
	}
}
func TestCookieInvalidDoesNotSilentlyLogin(t *testing.T) {
	for _, raw := range []string{"invalid", "[{bad json]", "[{\"name\":\"a\",\"domain\":\"evil.test\"}]", ""} {
		p := filepath.Join(t.TempDir(), "cookies.txt")
		os.WriteFile(p, []byte(raw), 0600)
		if _, e := loadCookies(p); e == nil {
			t.Fatal("invalid accepted")
		}
	}
}
func TestJarTracksRefreshAndDeletion(t *testing.T) {
	j := newJar()
	u, _ := url.Parse("https://mooc1.chaoxing.com/cards/page")
	j.SetCookies(u, []*http.Cookie{{Name: "_uid", Value: "new", Path: "/", MaxAge: 120}})
	if v, _ := j.value("_uid"); v != "new" {
		t.Fatal("refresh missing")
	}
	j.SetCookies(u, []*http.Cookie{{Name: "_uid", Value: "", Path: "/", MaxAge: -1}})
	if v, _ := j.value("_uid"); v != "" {
		t.Fatal("deletion missing")
	}
}
func TestAtomicWriteDoesNotExposeOldContents(t *testing.T) {
	p := filepath.Join(t.TempDir(), "cookies.txt")
	os.WriteFile(p, []byte("old"), 0600)
	if e := atomicWrite(p, []byte("new")); e != nil {
		t.Fatal(e)
	}
	b, _ := os.ReadFile(p)
	if string(b) != "new" {
		t.Fatal(string(b))
	}
	files, _ := filepath.Glob(filepath.Join(filepath.Dir(p), ".chaoxing-*"))
	if len(files) != 0 {
		t.Fatal("temp files remain")
	}
}
func TestHTTP403NeverRetried(t *testing.T) {
	var count atomic.Int32
	s, server := fixtureSession(t, func(w http.ResponseWriter, r *http.Request) { count.Add(1); http.Error(w, "Forbidden secret", 403) })
	_, e := s.get(context.Background(), server.URL, nil, nil, true)
	var f *Failure
	if !errors.As(e, &f) || f.Kind != "blocked" || retryable(e) || count.Load() != 1 {
		t.Fatal(e, count.Load())
	}
	if strings.Contains(e.Error(), "secret") {
		t.Fatal("response body leaked")
	}
}
func TestLoginPageNotGenericLoginWord(t *testing.T) {
	for _, tc := range []struct {
		body    string
		expired bool
	}{{`<button id="loginBtn">`, true}, {"course login instructions", false}} {
		s, server := fixtureSession(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, tc.body) })
		_, e := s.get(context.Background(), server.URL, nil, nil, true)
		if (e != nil) != tc.expired {
			t.Fatal(e)
		}
	}
}
func TestHTTPFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		status int
		kind   string
		retry  bool
	}{{401, "expired", false}, {403, "blocked", false}, {429, "blocked", false}, {404, "blocked", false}, {503, "server", true}, {302, "server", false}} {
		s, server := fixtureSession(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status) })
		_, e := s.get(context.Background(), server.URL, nil, nil, false)
		var f *Failure
		if !errors.As(e, &f) || f.Kind != tc.kind {
			t.Fatal(e)
		}
	}
}
func TestPostNotReplayedOnFailure(t *testing.T) {
	var n atomic.Int32
	s, server := fixtureSession(t, func(w http.ResponseWriter, r *http.Request) { n.Add(1); w.WriteHeader(503) })
	_, e := s.postForm(context.Background(), server.URL, url.Values{"a": {"b"}})
	if e == nil || n.Load() != 1 {
		t.Fatal(e, n.Load())
	}
}
func TestCancellationStopsRequest(t *testing.T) {
	s, server := fixtureSession(t, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, e := s.get(ctx, server.URL, nil, nil, true)
	if !errors.Is(e, context.DeadlineExceeded) {
		t.Fatal(e)
	}
}
func TestRedirectBounded(t *testing.T) {
	s, server := fixtureSession(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/loop", 302) })
	_, e := s.get(context.Background(), server.URL, nil, nil, false)
	if e == nil || retryable(e) {
		t.Fatal("redirect must fail permanently", e)
	}
}
func TestAESMatchesRoundtrip(t *testing.T) {
	encrypted := encryptCredential("中文password")
	data, e := base64.StdEncoding.DecodeString(encrypted)
	if e != nil {
		t.Fatal(e)
	}
	key := []byte("u2oh6Vu^HWe4_AES")
	block, _ := aes.NewCipher(key)
	plain := make([]byte, len(data))
	cipher.NewCBCDecrypter(block, key).CryptBlocks(plain, data)
	pad := int(plain[len(plain)-1])
	if string(plain[:len(plain)-pad]) != "中文password" {
		t.Fatal("AES incompatibility")
	}
}
func TestCoursesAndFoldersFields(t *testing.T) {
	b := []byte(`<div class="course" id="1"><input class="clazzId" value="class"><input class="courseId" value="course"><a href="/study?cpi=cpi&amp;x=1">go</a><span class="course-name" title="课程">课程</span></div>`)
	cs, e := parseCourses(b)
	if e != nil || len(cs) != 1 || cs[0].CPI != "cpi" {
		t.Fatal(cs, e)
	}
	if _, e = parseCourses([]byte(`<div class="course"></div>`)); e == nil {
		t.Fatal("invalid fields accepted")
	}
}
func TestPointsNestedNoDuplicates(t *testing.T) {
	b := []byte(`<div class="chapter_unit"><li><div id="cur123"><a class="clicktitle">章节</a><input class="knowledgeJobCount" value="3"><span class="bntHoverTips">已完成</span></div><ul><li><div id="cur456"><a class="clicktitle">子章节</a><span class="bntHoverTips">解锁</span></div></li></ul></li></div>`)
	ps, e := parsePoints(b)
	if e != nil || len(ps) != 2 || !ps[0].Finished || !ps[1].Locked {
		t.Fatal(ps, e)
	}
}
func TestCourseSelectionNoAllCoursesFallback(t *testing.T) {
	cs := []Course{{ID: "1", ClassID: "a"}, {ID: "1", ClassID: "b"}}
	out, e := selectCourses(cs, []string{"1"})
	if e != nil || len(out) != 2 {
		t.Fatal(out, e)
	}
	for _, ids := range [][]string{nil, {"2"}, {"1", "2"}} {
		if _, e = selectCourses(cs, ids); e == nil {
			t.Fatal("invalid selection accepted")
		}
	}
}
func TestCardsTypesAndDefaults(t *testing.T) {
	b := []byte(`mArg = {"defaults":{"knowledgeid":"node"},"attachments":[{"type":"video","job":true,"jobid":"v","mid":"m","objectId":"o","property":{"name":"two words","rt":"1"}},{"type":"document","job":true,"jobid":"d"},{"type":"workid","job":true,"jobid":"w"},{"type":"read","jobid":"r","property":{}},{"type":"video","job":true,"isPassed":true},{"type":"video","job":true,"property":{"liveId":"live"}}]};`)
	jobs, d, empty, e := parseCards(b)
	if e != nil || len(jobs) != 5 || empty || str(d["knowledgeid"]) != "node" || jobs[0].p("name") != "two words" || jobs[4].Kind != "live" {
		t.Fatal(jobs, d, empty, e)
	}
}
func TestCardEmptyNotOpenUnknown(t *testing.T) {
	if _, _, empty, e := parseCards([]byte("暂无内容")); e != nil || !empty {
		t.Fatal(e)
	}
	if _, _, _, e := parseCards([]byte("章节未开放")); e == nil {
		t.Fatal("notopen ignored")
	}
	for _, b := range []string{"", "<html>Unknown</html>", "mArg={bad json};", "mArg={\"attachments\":{}};"} {
		if _, _, _, e := parseCards([]byte(b)); e == nil {
			t.Fatal("format accepted")
		}
	}
}
func TestChapter403NotSuccessAndNoRetry(t *testing.T) {
	var requests atomic.Int32
	s, _ := fixtureSession(t, func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(403) })
	runner := Runner{Session: s, Config: Config{Jobs: 4, MaxAttempts: 5}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	results := runner.run(ctx, []ChapterTask{{Point: Point{ID: "1", Title: "章节"}}})
	if len(results) != 1 || results[0].Outcome != Failed || results[0].Attempts != 1 || requests.Load() != 1 {
		t.Fatal(results, requests.Load())
	}
}
func TestNotOpenSkipAndFiniteRetry(t *testing.T) {
	for _, action := range []string{"continue", "retry"} {
		s, _ := fixtureSession(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "章节未开放") })
		runner := Runner{Session: s, Config: Config{Jobs: 1, MaxAttempts: 2, RetryInterval: time.Millisecond, Common: map[string]string{"notopen_action": action}}}
		results := runner.run(context.Background(), []ChapterTask{{Point: Point{ID: "1"}}})
		if action == "continue" && results[0].Outcome != Skipped || action == "retry" && results[0].Attempts != 2 {
			t.Fatal(results)
		}
	}
}
func TestWorkerCompletionAndSummary(t *testing.T) {
	runner := Runner{Config: Config{Jobs: 4, MaxAttempts: 5}}
	tasks := []ChapterTask{}
	for i := 0; i < 32; i++ {
		tasks = append(tasks, ChapterTask{Point: Point{Finished: true}})
	}
	results := runner.run(context.Background(), tasks)
	if len(results) != len(tasks) {
		t.Fatal("tasks lost")
	}
	for _, r := range results {
		if r.Outcome != Completed {
			t.Fatal(r)
		}
	}
	if !strings.Contains(summary(results, 32), "完成 32") {
		t.Fatal("summary")
	}
}
func TestEmptyPageFailureNotSuccess(t *testing.T) {
	s, _ := fixtureSession(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "knowledge/cards") {
			io.WriteString(w, "暂无内容")
		} else {
			w.WriteHeader(403)
		}
	})
	runner := Runner{Session: s}
	outcome, e := runner.chapter(context.Background(), ChapterTask{Point: Point{ID: "1"}})
	if e == nil || outcome != Failed {
		t.Fatal(outcome, e)
	}
}
func TestSignaturePythonFixture(t *testing.T) {
	got := progressSignature("class", "user", "job", "object", 10, 120)
	h := md5.Sum([]byte("[class][user][job][object][10000][d_yHJ!$pdA~5][120000][0_120]"))
	if got != hex.EncodeToString(h[:]) {
		t.Fatal("signature mismatch")
	}
}
func TestVideo403Stops(t *testing.T) {
	s, _ := fixtureSession(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/status/") {
			io.WriteString(w, `{"status":"success","duration":1,"dtoken":"token","video":{}}`)
		} else {
			w.WriteHeader(403)
		}
	})
	s.Jar.add(storedCookie{Name: "_uid", Value: "123", Domain: ".chaoxing.com", Path: "/"})
	j := Job{Kind: "video", Raw: map[string]any{"jobid": "j", "objectId": "o"}, Property: map[string]any{"rt": "1"}}
	e := s.video(context.Background(), Course{ClassID: "class", CPI: "cpi"}, j, nil, 1)
	if e == nil || retryable(e) {
		t.Fatal(e)
	}
}
func TestUnknownTaskFailsClosed(t *testing.T) {
	s, _ := fixtureSession(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `mArg={"attachments":[{"type":"unknown","job":true}],"defaults":{}};`)
	})
	runner := Runner{Session: s}
	outcome, e := runner.chapter(context.Background(), ChapterTask{})
	if outcome != Failed || e == nil {
		t.Fatal(outcome, e)
	}
}
func TestQuestionFormsAndAnswers(t *testing.T) {
	form, questions, e := parseQuestions([]byte(`<form><input name="workId" value="w"><input name="enc" value="keep"><input name="answer1" value="old"><div class="singleQuesId" data="1"><div class="TiMu" data="0"><div class="Zy_TItle">题目</div><ul><li aria-label="A. 选项甲 选择">甲</li><li>B. 选项乙</li></ul></div></div></form>`))
	if e != nil || len(questions) != 1 || form.Get("enc") != "keep" || form.Get("answer1") != "" || form.Get("answerwqbid") != "1," {
		t.Fatal(form, questions, e)
	}
	answer, okay := convertAnswer(questions[0], "选项乙")
	if !okay || answer != "B" {
		t.Fatal(answer, okay)
	}
}
func TestAnswerConversion(t *testing.T) {
	tests := []struct {
		kind, answer, want string
		okay               bool
	}{{"single", "A", "A", true}, {"single", "AB", "", false}, {"multiple", "B;A;A", "AB", true}, {"judgement", "正确", "true", true}, {"judgement", "F", "false", true}, {"judgement", "不知道", "", false}, {"shortanswer", "答案", "答案", true}, {"single", "无匹配", "", false}}
	for _, tc := range tests {
		answer, okay := convertAnswer(Question{Kind: tc.kind, Options: []string{"A.甲", "B.乙"}}, tc.answer)
		if answer != tc.want || okay != tc.okay {
			t.Fatal(tc, answer, okay)
		}
	}
}
func TestRecordParser(t *testing.T) {
	times, score, found := parseRecords([]byte(`<span class="viewNum">第1次</span><span class="viewScore">60分</span><span class="viewNum">第2次</span><span class="viewScore">100分</span>`))
	if !found || times != 2 || score != 100 {
		t.Fatal(times, score, found)
	}
	if _, _, found = parseRecords([]byte(`<span class="viewNum">第1次</span>`)); found {
		t.Fatal("misaligned scores accepted")
	}
}
func TestGlyphHash(t *testing.T) {
	g := make([]byte, 16)
	binary.BigEndian.PutUint16(g[0:2], 1)
	binary.BigEndian.PutUint16(g[10:12], 0)
	binary.BigEndian.PutUint16(g[12:14], 0)
	g[14] = 0x31
	h := md5.Sum([]byte("001"))
	if glyphHash(g) != hex.EncodeToString(h[:]) {
		t.Fatal("glyph hash differs from Python")
	}
}
func TestFontMalformedFailsClosed(t *testing.T) {
	for _, page := range []string{`<style id="cxSecretStyle">bad</style>`, `<style id="cxSecretStyle">base64,AAAA</style>`} {
		if _, e := fontMap([]byte(page)); e == nil {
			t.Fatal("invalid font accepted")
		}
	}
}
func TestRedaction(t *testing.T) {
	s := redact("https://example.com/secret?key=abc password=secret Cookie: private")
	for _, token := range []string{"secret", "private", "abc"} {
		if strings.Contains(s, token) {
			t.Fatal("secret leaked", s)
		}
	}
}
func TestJarConcurrentAccess(t *testing.T) {
	j := newJar()
	u, _ := url.Parse("https://mooc1.chaoxing.com/")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 20; n++ {
				j.SetCookies(u, []*http.Cookie{{Name: "a", Value: "b"}})
				j.Cookies(u)
				j.value("a")
			}
		}()
	}
	wg.Wait()
}
func TestVersionDoesNotReadConfig(t *testing.T) {
	if code := execute(context.Background(), []string{"--version"}); code != 0 {
		t.Fatal(code)
	}
}
func TestManualBatch(t *testing.T) {
	a := &AnswerEngine{Config: Config{Tiku: map[string]string{"manual_mode_separator": ";"}}, Input: strings.NewReader("A;正确\n")}
	answers, e := a.manualBatch([]Question{{Title: "1"}, {Title: "2"}})
	if e != nil || len(answers) != 2 || answers[1] != "正确" {
		t.Fatal(answers, e)
	}
}
func TestAIResponseAndRequestWithoutCookies(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "" {
			t.Error("platform cookie leaked to provider")
		}
		if r.Header.Get("Authorization") != "Bearer test" {
			t.Error("missing auth")
		}
		var payload map[string]any
		if json.NewDecoder(r.Body).Decode(&payload) != nil {
			t.Error("invalid payload")
		}
		io.WriteString(w, `{"choices":[{"message":{"content":"","reasoning_content":"{\"Answer\":[\"选项甲\"]}"}}]}`)
	}))
	defer server.Close()
	a, e := newAnswers(Config{Path: filepath.Join(t.TempDir(), "config.ini"), Tiku: map[string]string{"provider": "AI", "endpoint": server.URL, "key": "test", "model": "test", "min_interval_seconds": "0"}})
	if e != nil {
		t.Fatal(e)
	}
	answer, e := a.query(context.Background(), Question{Kind: "single", Title: "question"}, "")
	if e != nil || answer != "选项甲" {
		t.Fatal(answer, e)
	}
}
func TestSavedWorkIsPending(t *testing.T) {
	var postCount atomic.Int32
	s, _ := fixtureSession(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "api/work"):
			io.WriteString(w, `<form><input name="workId" value="w"><div class="singleQuesId" data="1"><div class="TiMu" data="3"><div class="Zy_TItle">题目</div></div></div></form>`)
		case strings.Contains(r.URL.Path, "addStudentWorkNew"):
			postCount.Add(1)
			r.ParseForm()
			if r.Form.Get("pyFlag") != "1" {
				t.Error("unexpected submit")
			}
			io.WriteString(w, `{"status":true}`)
		}
	})
	c := Config{Path: filepath.Join(t.TempDir(), "config.ini"), Tiku: map[string]string{"provider": "TikuManual", "submit": "false", "manual_mode_default": "batch"}, Common: map[string]string{}}
	a, e := newAnswers(c)
	if e != nil {
		t.Fatal(e)
	}
	a.Input = bytes.NewBufferString("正确\n")
	runner := Runner{Session: s, Config: c, Answers: a}
	out, e := runner.work(context.Background(), ChapterTask{Point: Point{ID: "1"}}, Job{Raw: map[string]any{"jobid": "work-1"}}, nil)
	if e != nil || out != Pending || postCount.Load() != 1 {
		t.Fatal(out, e, postCount.Load())
	}
}
func FuzzCardsParser(f *testing.F) {
	for _, seed := range []string{"暂无内容", "章节未开放", `mArg={"attachments":[]};`, "unexpected"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data string) { parseCards([]byte(data)) })
}
func FuzzFontParser(f *testing.F) {
	f.Add(`<style id="cxSecretStyle">base64,AAAA</style>`)
	f.Add("plain")
	f.Fuzz(func(t *testing.T, page string) { fontMap([]byte(page)) })
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func fakeResponse(r *http.Request, status int, b string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(b)), Request: r}
}
func TestPasswordRedirectOneAttempt(t *testing.T) {
	s := newSession(newJar(), filepath.Join(t.TempDir(), "cookies.txt"))
	var n int
	s.Client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		n++
		resp := fakeResponse(r, 302, "")
		resp.Header.Set("Location", "https://passport2.chaoxing.com/login")
		return resp, nil
	})
	e := s.login(context.Background(), Config{Common: map[string]string{"username": "fake", "password": "fake"}})
	var f *Failure
	if !errors.As(e, &f) || f.Kind != "redirect" || n != 1 {
		t.Fatal(e, n)
	}
}
func TestCookieLoginDoesNotFallbackAndPreservesRefresh(t *testing.T) {
	for _, status := range []int{200, 403} {
		s := newSession(newJar(), filepath.Join(t.TempDir(), "cookies.txt"))
		s.Jar.add(storedCookie{Name: "_uid", Value: "123", Domain: ".chaoxing.com", Path: "/"})
		n := 0
		s.Client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
			n++
			if r.URL.Path == "/fanyalogin" {
				t.Error("unexpected password fallback")
			}
			res := fakeResponse(r, status, "courses")
			res.Header.Add("Set-Cookie", "newSession=refreshed; Domain=.chaoxing.com; Path=/; Secure; HttpOnly")
			return res, nil
		})
		e := s.login(context.Background(), Config{Common: map[string]string{"use_cookies": "true", "username": "unused", "password": "unused"}})
		if status == 200 && e != nil || status == 403 && e == nil || n != 1 {
			t.Fatal(e, n)
		}
		if status == 200 {
			j, e := loadCookies(s.CookiePath)
			if e != nil {
				t.Fatal(e)
			}
			v, _ := j.value("newSession")
			if v != "refreshed" {
				t.Fatal("session refresh not persisted")
			}
		}
	}
}
func TestGoProviderStringData(t *testing.T) {
	c := Config{Path: filepath.Join(t.TempDir(), "config.ini"), Tiku: map[string]string{}}
	a, _ := newAnswers(c)
	a.Session.Client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return fakeResponse(r, 200, `{"code":1,"data":"答案文本"}`), nil
	})
	answer, e := a.provider(context.Background(), "TikuGo", Question{Title: "test"}, "")
	if e != nil || answer != "答案文本" {
		t.Fatal(answer, e)
	}
}
func TestEnglishOptionMatching(t *testing.T) {
	answer, ok := convertAnswer(Question{Kind: "single", Options: []string{"A. alpha", "B. beta"}}, "alpha")
	if !ok || answer != "A" {
		t.Fatal(answer, ok)
	}
}
func TestWork403NeverSubmitted(t *testing.T) {
	var n atomic.Int32
	s, _ := fixtureSession(t, func(w http.ResponseWriter, r *http.Request) { n.Add(1); w.WriteHeader(403) })
	runner := Runner{Session: s, Config: Config{Tiku: map[string]string{"provider": "AI"}}}
	out, e := runner.work(context.Background(), ChapterTask{}, Job{Raw: map[string]any{"jobid": "work-1"}}, nil)
	if out != Failed || e == nil || n.Load() != 1 {
		t.Fatal(out, e, n.Load())
	}
}
func TestProviderBlockedNotRetried(t *testing.T) {
	a, _ := newAnswers(Config{Path: filepath.Join(t.TempDir(), "config.ini"), Tiku: map[string]string{"go_retry_times": "3", "go_retry_backoff": "0"}})
	n := 0
	a.Session.Client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) { n++; return fakeResponse(r, 429, "secret"), nil })
	_, e := a.providerRetry(context.Background(), "TikuGo", Question{Title: "test"}, "")
	if e == nil || n != 1 {
		t.Fatal(e, n)
	}
}
func TestProviderNetworkRetriesBounded(t *testing.T) {
	a, _ := newAnswers(Config{Path: filepath.Join(t.TempDir(), "config.ini"), Tiku: map[string]string{"go_retry_times": "3", "go_retry_backoff": "0"}})
	n := 0
	a.Session.Client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) { n++; return nil, errors.New("network") })
	_, e := a.providerRetry(context.Background(), "TikuGo", Question{Title: "test"}, "")
	if e == nil || n != 3 {
		t.Fatal(e, n)
	}
}
func TestNoUnconfirmedCompletionAfterDocument(t *testing.T) {
	s, _ := fixtureSession(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "knowledge/cards") {
			if r.URL.Query().Get("num") == "0" {
				io.WriteString(w, `mArg={"attachments":[{"type":"document","job":true,"jobid":"d"}]};`)
			} else {
				io.WriteString(w, "暂无内容")
			}
		} else {
			io.WriteString(w, "ok")
		}
	})
	runner := Runner{Session: s}
	out, e := runner.chapter(context.Background(), ChapterTask{})
	if e != nil || out != Pending {
		t.Fatal(out, e)
	}
}
