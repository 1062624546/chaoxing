package main

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"golang.org/x/net/html"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Outcome string

const (
	Completed Outcome = "completed"
	Pending   Outcome = "pending"
	Skipped   Outcome = "skipped"
	Failed    Outcome = "failed"
)

type ChapterTask struct {
	Course Course
	Point  Point
}
type TaskResult struct {
	Task     ChapterTask
	Outcome  Outcome
	Err      error
	Attempts int
}
type Runner struct {
	Session         *Session
	Config          Config
	Answers         *AnswerEngine
	ChapterInterval time.Duration
}

func (r *Runner) chapter(ctx context.Context, t ChapterTask) (Outcome, error) {
	if t.Point.Finished {
		return Completed, nil
	}
	jobs, defaults, e := r.Session.jobs(ctx, t.Course, t.Point)
	if e != nil {
		return Failed, e
	}
	if len(jobs) == 0 {
		_, e = r.Session.emptyPage(ctx, t.Course, t.Point)
		if e != nil {
			return Failed, e
		}
		return Completed, nil
	}
	pending := false
	for _, j := range jobs {
		if e := ctx.Err(); e != nil {
			return Failed, e
		}
		info("处理任务类型：%s", j.Kind)
		var outcome Outcome
		switch j.Kind {
		case "video", "audio":
			e = r.Session.video(ctx, t.Course, j, defaults, r.Config.Speed)
			outcome = Completed
		case "document":
			e = r.Session.document(ctx, t.Course, t.Point, j)
			outcome = Completed
		case "read":
			e = r.Session.read(ctx, t.Course, t.Point, j, defaults)
			outcome = Completed
		case "live":
			e = r.Session.live(ctx, t.Course, t.Point, j, defaults, r.Config.Speed)
			outcome = Completed
		case "workid":
			outcome, e = r.work(ctx, t, j, defaults)
		default:
			return Failed, fail("format", "未知任务类型，不能标记成功", 0)
		}
		if e != nil {
			return Failed, e
		}
		if outcome == Pending {
			pending = true
		}
	}
	if pending {
		return Pending, nil
	}
	// Confirm completion from the platform rather than treating a 200 response as completion.
	remaining, _, e := r.Session.jobs(ctx, t.Course, t.Point)
	if e != nil {
		return Failed, e
	}
	if len(remaining) > 0 {
		return Pending, nil
	}
	return Completed, nil
}
func (r *Runner) run(ctx context.Context, tasks []ChapterTask) []TaskResult {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var failures atomic.Int32
	var startMu sync.Mutex
	var lastStart time.Time
	queue := make(chan ChapterTask)
	results := make(chan TaskResult, len(tasks))
	var wg sync.WaitGroup
	workers := r.Config.Jobs
	if workers < 1 {
		workers = 1
	}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for task := range queue {
				if ctx.Err() != nil {
					return
				}

				res := TaskResult{Task: task, Outcome: Failed}
				for attempt := 1; attempt <= r.Config.MaxAttempts; attempt++ {
					if ctx.Err() != nil {
						break
					}
					if !task.Point.Finished {
						startMu.Lock()
						e := sleepContext(ctx, time.Until(lastStart.Add(r.ChapterInterval)))
						if e == nil {
							lastStart = time.Now()
						}
						startMu.Unlock()
						if e != nil {
							res.Err = e
							break
						}
					}
					res.Attempts = attempt
					res.Outcome, res.Err = r.chapter(ctx, task)
					if res.Err == nil {
						break
					}
					var f *Failure
					if errors.As(res.Err, &f) && f.Kind == "notopen" {
						if r.Config.Common["notopen_action"] == "continue" {
							res.Outcome = Skipped
							res.Err = nil
							break
						}
					} else {
						if !errors.Is(res.Err, context.Canceled) && failures.Add(1) >= 3 {
							info("本次运行累计3次错误，停止整个任务队列")
							cancel()
							break
						}
						if !retryable(res.Err) {
							break
						}
					}
					if attempt < r.Config.MaxAttempts {
						if e := sleepContext(ctx, r.Config.RetryInterval); e != nil {
							res.Err = e
							break
						}
					}
				}
				results <- res
			}
		}()
	}
	go func() {
		defer close(queue)
		for _, t := range tasks {
			select {
			case queue <- t:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() { wg.Wait(); close(results) }()
	out := []TaskResult{}
	for res := range results {
		info("章节状态：%s — %s", res.Task.Point.Title, res.Outcome)
		if res.Err != nil {
			info("章节失败：%v", res.Err)
		}
		out = append(out, res)
	}
	return out
}
func (s *Session) emptyPage(ctx context.Context, c Course, p Point) ([]byte, error) {
	return s.get(ctx, s.platform("/mooc-ans/mycourse/studentstudyAjax"), qv(map[string]string{"courseId": c.ID, "clazzid": c.ClassID, "chapterId": p.ID, "cpi": c.CPI, "verificationcode": "", "mooc2": "1", "microTopicId": "0", "editorPreview": "0"}), nil, false)
}
func (s *Session) document(ctx context.Context, c Course, p Point, j Job) error {
	node := p.ID
	if m := regexp.MustCompile(`nodeId_([0-9]+)-`).FindStringSubmatch(j.v("otherInfo")); len(m) > 0 {
		node = m[1]
	}
	_, e := s.get(ctx, s.platform("/ananas/job/document"), qv(map[string]string{"jobid": j.v("jobid"), "knowledgeid": node, "courseid": c.ID, "clazzid": c.ClassID, "jtoken": j.v("jtoken"), "_dc": timestamp()}), nil, false)
	return e
}
func (s *Session) read(ctx context.Context, c Course, p Point, j Job, d map[string]any) error {
	b, e := s.get(ctx, s.platform("/ananas/job/readv2"), qv(map[string]string{"jobid": j.v("jobid"), "knowledgeid": p.ID, "jtoken": j.v("jtoken"), "courseid": c.ID, "clazzid": c.ClassID}), nil, false)
	if e != nil {
		return e
	}
	var result map[string]any
	if json.Unmarshal(b, &result) != nil {
		return fail("format", "阅读接口返回无效 JSON", 0)
	}
	if result["status"] != nil && !truth(result["status"]) {
		return fail("task", "阅读任务未被平台接受", 0)
	}
	return nil
}
func timestamp() string { return fmt.Sprint(time.Now().UnixMilli()) }
func progressSignature(class, user, job, object string, playing, duration int) string {
	data := fmt.Sprintf("[%s][%s][%s][%s][%d][d_yHJ!$pdA~5][%d][0_%d]", class, user, job, object, playing*1000, duration*1000, duration)
	h := md5.Sum([]byte(data))
	return hex.EncodeToString(h[:])
}
func (s *Session) video(ctx context.Context, c Course, j Job, d map[string]any, speed float64) error {
	fid, e := s.Jar.value("fid")
	if e != nil {
		return e
	}
	if fid == "" {
		fid = "1024"
	}
	uid, e := s.Jar.value("_uid")
	if e != nil {
		return e
	}
	if uid == "" {
		uid, e = s.Jar.value("UID")
	}
	if e != nil || uid == "" {
		return fail("expired", "视频任务缺少用户身份", 0)
	}
	kind := "Video"
	ref := s.platform("/ananas/modules/video/index.html?v=2025-0725-1842")
	var meta map[string]any
	e = s.getJSON(ctx, s.platform("/ananas/status/"+url.PathEscape(j.v("objectId"))), qv(map[string]string{"k": fid, "flag": "normal"}), map[string]string{"Referer": ref}, &meta)
	if e != nil {
		return e
	}
	if str(meta["status"]) != "success" {
		return fail("task", "媒体状态未就绪", 0)
	}
	if j.Kind == "audio" || meta["video"] == nil && meta["audio"] != nil {
		kind = "Audio"
		ref = s.platform("/ananas/modules/audio/index_new.html?v=2025-0725-1842")
	}
	duration := int(number(str(meta["duration"]), 0))
	token := str(meta["dtoken"])
	if duration <= 0 || token == "" || j.v("jobid") == "" || j.v("objectId") == "" {
		return fail("format", "媒体任务元数据缺失", 0)
	}
	rt := j.p("rt")
	if rt == "" {
		m := regexp.MustCompile(`-rt_([1d])`).FindStringSubmatch(j.v("otherInfo"))
		if len(m) > 0 {
			rt = "1"
			if m[1] == "d" {
				rt = "0.9"
			}
		}
	}
	if rt == "" {
		return fail("format", "缺少媒体 rt 参数，停止猜测参数", 0)
	}
	initial := int(number(j.v("playTime"), 0) / 1000)
	if initial > duration {
		initial = duration
	}
	start := time.Now()
	info("媒体任务时长：%d秒，已有进度：%d秒", duration, initial)
	for {
		playing := initial + int(time.Since(start).Seconds()*speed)
		if playing > duration {
			playing = duration
		}
		q := qv(map[string]string{"clazzId": c.ClassID, "playingTime": str(playing), "duration": str(duration), "clipTime": "0_" + str(duration), "objectId": j.v("objectId"), "otherInfo": j.v("otherInfo"), "courseId": c.ID, "jobid": j.v("jobid"), "userid": uid, "isdrag": "3", "view": "pc", "enc": progressSignature(c.ClassID, uid, j.v("jobid"), j.v("objectId"), playing, duration), "dtype": kind, "rt": rt, "_t": timestamp()})
		for _, k := range []string{"videoFaceCaptureEnc", "attDuration", "attDurationEnc"} {
			if j.v(k) != "" {
				q.Set(k, j.v(k))
			}
		}
		b, e := s.get(ctx, s.platform("/mooc-ans/multimedia/log/a/"+url.PathEscape(c.CPI)+"/"+url.PathEscape(token)), q, map[string]string{"Referer": ref}, false)
		if e != nil {
			return e
		}
		if strings.Contains(string(b), "验证码") {
			return fail("blocked", "平台要求验证码，请在官方网页处理", 0)
		}
		var res map[string]any
		if json.Unmarshal(b, &res) != nil {
			return fail("format", "媒体进度返回非 JSON", 0)
		}
		if truth(res["isPassed"]) {
			return nil
		}
		if playing == duration {
			return fail("task", "已到媒体末尾，但平台尚未确认通过", 0)
		}
		wait := 30 * time.Second
		remaining := time.Duration(float64(duration-playing) / speed * float64(time.Second))
		if wait > remaining {
			wait = remaining
		}
		if e := sleepContext(ctx, wait); e != nil {
			return e
		}
	}
}
func (s *Session) live(ctx context.Context, c Course, p Point, j Job, d map[string]any, speed float64) error {
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
	if j.p("liveId") == "" || j.p("streamName") == "" || j.p("vdoid") == "" || uid == "" {
		return fail("format", "直播任务参数不完整", 0)
	}
	var status map[string]any
	if e = s.getJSON(ctx, s.platform("/ananas/live/liveinfo"), qv(map[string]string{"liveid": j.p("liveId"), "userid": uid, "clazzid": c.ClassID, "knowledgeid": p.ID, "courseid": c.ID, "jobid": j.p("_jobid"), "ut": "s"}), nil, &status); e != nil {
		return e
	}
	duration := number(str(object(object(status["temp"])["data"])["duration"]), 0)
	if duration <= 0 {
		return fail("format", "直播总时长未知，不能虚构时长", 0)
	}
	deadline := time.Now().Add(time.Duration(duration / speed * float64(time.Second)))
	for {
		b, e := s.get(ctx, "https://zhibo.chaoxing.com/saveTimePc", qv(map[string]string{"streamName": j.p("streamName"), "vdoid": j.p("vdoid"), "userId": uid, "isStart": "0", "t": timestamp(), "courseId": c.ID}), nil, false)
		if e != nil {
			return e
		}
		if strings.TrimSpace(string(b)) != "@success" {
			return fail("task", "平台未接受直播记录", 0)
		}
		if time.Now().After(deadline) {
			return nil
		}
		wait := 59 * time.Second
		remaining := time.Until(deadline)
		if wait > remaining {
			wait = remaining
		}
		if e := sleepContext(ctx, wait); e != nil {
			return e
		}
	}
}
func (s *Session) increaseCount(ctx context.Context, c Course, p Point, count int) error {
	for i := 0; i < count; i++ {
		b, e := s.emptyPage(ctx, c, p)
		if e != nil {
			return e
		}
		root, e := parseDOM(b)
		if e != nil {
			return e
		}
		for _, n := range collect(root, func(x *html.Node) bool { return x.Data == "script" }) {
			src := attr(n, "src")
			u, e := url.Parse(src)
			if e != nil || u.Scheme != "https" || u.Hostname() != "fystat-ans.chaoxing.com" || u.Path != "/log/setlog" {
				continue
			}
			if _, e = s.get(ctx, u.String(), nil, nil, false); e != nil {
				return e
			}
		}
		if e = sleepContext(ctx, time.Second); e != nil {
			return e
		}
	}
	return nil
}
