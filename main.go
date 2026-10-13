package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

var version = "go-dev"

func execute(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("chaoxing", flag.ContinueOnError)
	var path, username, password, courses, action string
	var cookies, list, check, showVersion, autoSign, addCount, verbose bool
	var jobs, target int
	var speed, retryInterval float64
	fs.StringVar(&path, "c", "config.ini", "配置文件路径")
	fs.StringVar(&path, "config", "config.ini", "配置文件路径")
	fs.StringVar(&username, "u", "", "手机号账号（建议配置文件）")
	fs.StringVar(&password, "p", "", "密码（建议配置文件）")
	fs.StringVar(&courses, "l", "", "课程 ID，以逗号分隔")
	fs.BoolVar(&cookies, "use-cookies", false, "使用 Cookie 登录")
	fs.IntVar(&jobs, "j", 0, "章节并发数量")
	fs.Float64Var(&speed, "s", 0, "媒体播放速度，1至2")
	fs.StringVar(&action, "notopen-action", "", "未开放章节：retry/continue")
	fs.Float64Var(&retryInterval, "retry-interval", -1, "重试间隔秒数")
	fs.BoolVar(&list, "list-courses", false, "登录后只列出课程")
	fs.BoolVar(&check, "check-login", false, "只验证登录，不处理任务")
	fs.BoolVar(&showVersion, "version", false, "显示版本")
	fs.BoolVar(&autoSign, "auto-sign", false, "普通签到，不处理验证码/位置签到")
	fs.BoolVar(&addCount, "add-learning-count", false, "增加章节访问次数")
	fs.IntVar(&target, "target-count", 0, "章节访问次数")
	fs.BoolVar(&verbose, "v", false, "兼容旧调试参数（日志仍隐藏凭据）")
	fs.StringVar(&username, "username", "", "兼容原命令行参数")
	fs.StringVar(&password, "password", "", "兼容原命令行参数")
	fs.StringVar(&courses, "list", "", "兼容原命令行参数")
	fs.IntVar(&jobs, "jobs", 0, "兼容原命令行参数")
	fs.Float64Var(&speed, "speed", 0, "兼容原命令行参数")
	fs.BoolVar(&verbose, "verbose", false, "兼容原命令行参数")
	fs.BoolVar(&verbose, "debug", false, "兼容原命令行参数")
	if e := fs.Parse(args); e != nil {
		if errors.Is(e, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if showVersion {
		fmt.Println("chaoxing " + version)
		return 0
	}
	if fs.NArg() > 0 {
		info("存在无法识别的位置参数")
		return 2
	}
	c, e := loadConfig(path)
	if e != nil {
		info("配置失败：%v", e)
		return 2
	}
	if username != "" {
		c.Common["username"] = username
	}
	if password != "" {
		c.Common["password"] = password
	}
	if cookies {
		c.Common["use_cookies"] = "true"
	}
	if courses != "" {
		c.Common["course_list"] = courses
	}
	if jobs != 0 {
		if jobs < 1 || jobs > 64 {
			info("并发参数无效")
			return 2
		}
		c.Jobs = jobs
	}
	if speed != 0 {
		if math.IsNaN(speed) || math.IsInf(speed, 0) || speed < 1 || speed > 2 {
			info("播放速度参数无效")
			return 2
		}
		c.Speed = speed
	}
	if action != "" {
		if action != "retry" && action != "continue" {
			info("未开放处理参数无效")
			return 2
		}
		c.Common["notopen_action"] = action
	}
	if math.IsNaN(retryInterval) || math.IsInf(retryInterval, 0) || retryInterval > 3600 {
		info("重试间隔参数无效")
		return 2
	}
	if retryInterval >= 0 {
		c.RetryInterval = time.Duration(retryInterval * float64(time.Second))
	}
	if addCount {
		c.Common["add_learning_count"] = "true"
	}
	if target != 0 {
		c.Common["target_count"] = fmt.Sprint(target)
	}
	j := newJar()
	if yes(c.Common["use_cookies"]) {
		j, e = loadCookies(c.CookiePath)
		if e != nil {
			info("Cookie 加载失败：%v", e)
			return 1
		}
	}
	s := newSession(j, c.CookiePath)
	defer s.Client.CloseIdleConnections()
	if e = s.login(ctx, c); e != nil {
		info("登录失败：%v", e)
		return 1
	}
	defer func() {
		if e := s.Jar.save(s.CookiePath); e != nil {
			info("Cookie 保存失败：%v", e)
		}
	}()
	if check {
		return 0
	}
	cs, e := s.courses(ctx)
	if e != nil {
		info("读取课程失败：%v", e)
		return 1
	}
	if list {
		for _, course := range cs {
			fmt.Printf("ID: %s 班级ID: %s 课程名: %s\n", course.ID, course.ClassID, course.Title)
		}
		return 0
	}
	selected, e := selectCourses(cs, splitList(c.Common["course_list"]))
	if e != nil {
		info("课程选择失败：%v", e)
		return 2
	}
	answers, e := newAnswers(c)
	if e != nil {
		info("题库配置失败：%v", e)
		return 2
	}
	defer answers.Session.Client.CloseIdleConnections()
	if e = answers.check(ctx); e != nil {
		info("大模型连接检查失败：%v", e)
		return 1
	}
	tasks := []ChapterTask{}
	for _, course := range selected {
		points, e := s.points(ctx, course)
		if e != nil {
			info("读取章节失败：%v", e)
			return 1
		}
		for _, point := range points {
			tasks = append(tasks, ChapterTask{Course: course, Point: point})
		}
		if autoSign {
			if e = s.signNormal(ctx, course); e != nil {
				info("普通签到失败：%v", e)
				return 1
			}
		}
	}
	if len(tasks) == 0 {
		info("没有需要处理的章节")
		return 0
	}
	runner := Runner{Session: s, Config: c, Answers: answers}
	results := runner.run(ctx, tasks)
	message := summary(results, len(tasks))
	info("%s", message)
	notifyCtx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	if e = notify(notifyCtx, c, message); e != nil {
		info("通知发送失败：%v", e)
	}
	if ctx.Err() != nil {
		return 130
	}
	allCompleted := len(results) == len(tasks)
	for _, result := range results {
		if result.Outcome != Completed {
			allCompleted = false
		}
	}
	if yes(c.Common["add_learning_count"]) {
		if !allCompleted {
			info("存在未完成章节，停止增加访问次数")
			return 1
		}
		count := int(number(c.Common["target_count"], 100))
		if count < 1 || count > 10000 {
			info("target_count 应在1至10000之间")
			return 2
		}
		for _, course := range selected {
			courseTasks := []ChapterTask{}
			for _, task := range tasks {
				if task.Course.ID == course.ID && task.Course.ClassID == course.ClassID {
					courseTasks = append(courseTasks, task)
				}
			}
			if len(courseTasks) == 0 {
				continue
			}
			for i := 0; i < count; i++ {
				task := courseTasks[i%len(courseTasks)]
				if e = s.increaseCount(ctx, task.Course, task.Point, 1); e != nil {
					info("章节访问失败：%v", e)
					return 1
				}
			}
		}
	}
	if !allCompleted {
		return 1
	}
	return 0
}
func (s *Session) signNormal(ctx context.Context, c Course) error {
	fid, e := s.Jar.value("fid")
	if e != nil {
		return e
	}
	if fid == "" {
		fid = "1024"
	}
	var activities map[string]any
	if e := s.getJSON(ctx, "https://mobilelearn.chaoxing.com/v2/apis/active/student/activelist", qv(map[string]string{"fid": fid, "courseId": c.ID, "classId": c.ClassID, "showNotStartedActive": "0", "_": timestamp()}), nil, &activities); e != nil {
		return e
	}
	data := object(activities["data"])
	items, _ := data["activeList"].([]any)
	uid, e := s.Jar.value("_uid")
	if e != nil {
		return e
	}
	if !truth(activities["result"]) {
		return fail("task", "平台未接受活动查询", 0)
	}
	for _, item := range items {
		a := object(item)
		if str(a["status"]) != "1" || str(a["activeType"]) != "2" {
			continue
		}
		other := str(a["otherId"])
		if other != "" && other != "0" {
			info("非普通签到需在官方页面处理")
			continue
		}
		id := str(a["id"])
		if id == "" {
			return fail("format", "签到活动 ID 缺失", 0)
		}
		if _, e = s.get(ctx, "https://mobilelearn.chaoxing.com/newsign/preSign", qv(map[string]string{"general": "1", "sys": "1", "ls": "1", "appType": "15", "tid": "", "ut": "s", "uid": uid, "activePrimaryId": id, "courseId": c.ID, "classId": c.ClassID}), nil, false); e != nil {
			return e
		}
		b, e := s.get(ctx, "https://mobilelearn.chaoxing.com/pptSign/stuSignajax", qv(map[string]string{"activeId": id, "uid": uid, "fid": fid, "courseId": c.ID, "classId": c.ClassID, "clientip": "", "objectId": "aaa", "name": "", "useragent": "", "latitude": "-1", "longitude": "-1", "appType": "15"}), nil, false)
		if e != nil {
			return e
		}
		if !strings.Contains(string(b), "success") && !strings.Contains(string(b), "已签到") {
			return fail("task", "普通签到未被平台确认", 0)
		}
	}
	return nil
}
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	w, e := setupLog()
	if e != nil {
		fmt.Fprintln(os.Stderr, "日志文件无法打开，请检查目录权限")
		os.Exit(1)
	}
	code := execute(ctx, os.Args[1:])
	w.Close()
	os.Exit(code)
}
