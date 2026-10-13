package main

import (
	"context"
	"encoding/json"
	"fmt"
	"golang.org/x/net/html"
	"net/url"
	"regexp"
	"strings"
	"time"
)

func questionContent(n *html.Node) string {
	if n == nil {
		return ""
	}
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(x *html.Node) {
		if x.Type == html.TextNode {
			b.WriteString(x.Data)
		}
		if x.Type == html.ElementNode && x.Data == "img" {
			fmt.Fprintf(&b, `<img src="%s">`, attr(x, "src"))
		}
		for c := x.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return strings.TrimSpace(strings.NewReplacer("\n", "", "\r", "", "\t", "").Replace(b.String()))
}
func parseQuestions(b []byte) (url.Values, []Question, error) {
	root, e := parseDOM(b)
	if e != nil {
		return nil, nil, e
	}
	form := tag(root, "form")
	if form == nil {
		return nil, nil, fail("format", "测验页缺少表单，不能提交", 0)
	}
	mapping, e := fontMap(b)
	if e != nil {
		return nil, nil, e
	}
	values := url.Values{}
	for _, n := range collect(form, func(x *html.Node) bool { return x.Data == "input" }) {
		name := attr(n, "name")
		if name != "" && !strings.Contains(name, "answer") {
			values.Add(name, attr(n, "value"))
		}
	}
	questions := []Question{}
	ids := []string{}
	for _, n := range collect(form, func(x *html.Node) bool { return hasClass(x, "singleQuesId") }) {
		q := Question{ID: attr(n, "data"), Code: attr(class(n, "TiMu"), "data"), Title: decodeFontText(questionContent(class(n, "Zy_TItle")), mapping)}
		switch q.Code {
		case "0":
			q.Kind = "single"
		case "1":
			q.Kind = "multiple"
		case "2":
			q.Kind = "completion"
		case "3":
			q.Kind = "judgement"
		case "4":
			q.Kind = "shortanswer"
		default:
			return nil, nil, fail("format", "测验含未知题型，停止提交", 0)
		}
		if q.ID == "" || q.Title == "" {
			return nil, nil, fail("format", "测验题目字段不完整", 0)
		}
		ul := tag(n, "ul")
		for _, li := range collect(ul, func(x *html.Node) bool { return x.Data == "li" }) {
			opt := attr(li, "aria-label")
			if opt == "" {
				opt = text(li)
			}
			opt = strings.TrimSpace(strings.TrimSuffix(opt, "选择"))
			q.Options = append(q.Options, decodeFontText(opt, mapping))
		}
		questions = append(questions, q)
		ids = append(ids, q.ID)
	}
	if len(questions) == 0 {
		return nil, nil, fail("format", "测验没有可识别题目", 0)
	}
	values.Set("answerwqbid", strings.Join(ids, ",")+",")
	return values, questions, nil
}
func workQuery(c Course, p Point, j Job, d map[string]any) url.Values {
	cpi := str(d["cpi"])
	if cpi == "" {
		cpi = c.CPI
	}
	return qv(map[string]string{"api": "1", "workId": strings.TrimPrefix(j.v("jobid"), "work-"), "jobid": j.v("jobid"), "originJobId": j.v("jobid"), "needRedirect": "true", "skipHeader": "true", "knowledgeid": p.ID, "ktoken": str(d["ktoken"]), "cpi": cpi, "ut": "s", "clazzId": c.ClassID, "type": "", "enc": j.v("enc"), "mooc2": "1", "courseid": c.ID})
}
func (r *Runner) work(ctx context.Context, t ChapterTask, j Job, d map[string]any) (Outcome, error) {
	if len(splitList(r.Config.Tiku["provider"])) == 0 {
		info("未配置题库，测验留待手动处理")
		return Pending, nil
	}
	rounds := 1
	if yes(r.Config.Common["work_redo_enabled"]) {
		rounds += int(number(r.Config.Common["work_max_retries"], 3))
		if rounds < 1 || rounds > 11 {
			return Failed, fail("config", "work_max_retries 应在0至10之间", 0)
		}
	}
	feedback := ""
	for round := 0; round < rounds; round++ {
		b, e := r.Session.get(ctx, r.Session.platform("/mooc-ans/api/work"), workQuery(t.Course, t.Point, j, d), nil, true)
		if e != nil {
			return Failed, e
		}
		if strings.Contains(string(b), "教师未创建完成该测验") {
			return Pending, nil
		}
		form, questions, e := parseQuestions(b)
		if e != nil {
			return Failed, e
		}
		covered := 0
		answers := make([]string, len(questions))
		if r.Config.Tiku["provider"] == "TikuManual" && r.Config.Tiku["manual_mode_default"] != "single" {
			answers, e = r.Answers.manualBatch(questions)
			if e != nil {
				return Failed, e
			}
		} else {
			for i, q := range questions {
				answers[i], e = r.Answers.query(ctx, q, feedback)
				if e != nil {
					return Failed, e
				}
			}
		}
		for i, q := range questions {
			answer, found := convertAnswer(q, answers[i])
			form.Set("answertype"+q.ID, q.Code)
			if found {
				form.Set("answer"+q.ID, answer)
				covered++
			} else {
				form.Set("answer"+q.ID, "")
			}
		}
		cover := float64(covered) / float64(len(questions))
		threshold := number(r.Config.Tiku["cover_rate"], 0.9)
		if threshold < 0 || threshold > 1 {
			return Failed, fail("config", "cover_rate 应在0至1之间", 0)
		}
		submit := yes(r.Config.Tiku["submit"]) && cover >= threshold
		form.Set("pyFlag", "1")
		if submit {
			form.Set("pyFlag", "")
		}
		info("测验覆盖率：%.0f%%；提交：%t", cover*100, submit)
		result, e := r.Session.postForm(ctx, r.Session.platform("/mooc-ans/work/addStudentWorkNew"), form)
		if e != nil {
			return Failed, e
		}
		var ack map[string]any
		if json.Unmarshal(result, &ack) != nil {
			return Failed, fail("format", "测验保存/提交返回非 JSON", 0)
		}
		if !truth(ack["status"]) {
			return Failed, fail("task", "平台拒绝保存或提交测验", 0)
		}
		if !submit {
			return Pending, nil
		}
		score, report, e := r.Session.workResult(ctx, t, j, form)
		if e != nil {
			return Failed, e
		}
		info("测验提交后成绩：%.2f", score)
		if score >= 99.95 {
			return Completed, nil
		}
		if round+1 == rounds {
			return Pending, nil
		}
		feedback = report
		if feedback == "" {
			return Pending, nil
		}
		if e = sleepContext(ctx, 2*time.Second); e != nil {
			return Failed, e
		}
	}
	return Pending, nil
}

var recordNumberRE = regexp.MustCompile(`第(\d+)次`)
var scoreNumberRE = regexp.MustCompile(`([0-9]+(?:\.[0-9]+)?)分`)

func parseRecords(b []byte) (int, float64, bool) {
	root, e := parseDOM(b)
	if e != nil {
		return 0, 0, false
	}
	nums := collect(root, func(x *html.Node) bool { return hasClass(x, "viewNum") })
	scores := collect(root, func(x *html.Node) bool { return hasClass(x, "viewScore") })
	if len(nums) == 0 || len(nums) != len(scores) {
		return 0, 0, false
	}
	latest := -1
	score := 0.0
	for i, n := range nums {
		m := recordNumberRE.FindStringSubmatch(text(n))
		s := scoreNumberRE.FindStringSubmatch(text(scores[i]))
		if len(m) < 2 || len(s) < 2 {
			return 0, 0, false
		}
		times := int(number(m[1], 0))
		if times > latest {
			latest = times
			score = number(s[1], 0)
		}
	}
	return latest, score, true
}
func (s *Session) workResult(ctx context.Context, t ChapterTask, j Job, form url.Values) (float64, string, error) {
	id := form.Get("workId")
	if id == "" {
		id = form.Get("workRelationId")
	}
	if id == "" {
		id = strings.TrimPrefix(j.v("jobid"), "work-")
	}
	q := qv(map[string]string{"courseId": t.Course.ID, "classId": t.Course.ClassID, "workId": id, "workAnswerId": form.Get("workAnswerId"), "cpi": t.Course.CPI, "api": "1", "mooc2": "1", "ut": "s"})
	latest, score, found := 0, 0.0, false
	for attempt := 0; attempt < 5; attempt++ {
		b, e := s.get(ctx, s.platform("/mooc-ans/work/record-list"), q, nil, true)
		if e != nil {
			return 0, "", e
		}
		latest, score, found = parseRecords(b)
		if found {
			break
		}
		if attempt < 4 {
			if e = sleepContext(ctx, 1500*time.Millisecond); e != nil {
				return 0, "", e
			}
		}
	}
	if !found {
		return 0, "", fail("format", "测验已提交，但无法确认官方成绩，停止重做", 0)
	}
	if score >= 99.95 {
		return score, "", nil
	}
	q.Set("times", fmt.Sprint(latest))
	for k, v := range map[string]string{"isdisplaytable": "0", "firstHeader": "2", "isWork": "false", "workSystem": "0", "archive": "false"} {
		q.Set(k, v)
	}
	b, e := s.get(ctx, s.platform("/mooc-ans/work/record-detail"), q, nil, true)
	if e != nil {
		return 0, "", e
	}
	root, e := parseDOM(b)
	if e != nil {
		return 0, "", e
	}
	report := []string{}
	for _, n := range collect(root, func(x *html.Node) bool { return hasClass(x, "singleQuesId") }) {
		title := text(class(n, "newZy_TItle"))
		if title == "" {
			title = text(class(n, "Zy_TItle"))
		}
		values := collect(n, func(x *html.Node) bool { return hasClass(x, "answerCon") })
		if len(values) < 2 {
			continue
		}
		mine, correct := text(values[0]), text(values[1])
		if normalize(mine) != normalize(correct) {
			report = append(report, "题目："+title+"；我的答案："+mine+"；正确答案："+correct)
		}
	}
	return score, strings.Join(report, "\n"), nil
}
