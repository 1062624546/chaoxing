package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func notify(ctx context.Context, c Config, message string) error {
	address := c.Notification["url"]
	if address == "" {
		return nil
	}
	j := newJar()
	s := newSession(j, "")
	s.Client.Jar = nil
	s.Client.Timeout = 25 * time.Second
	s.Client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	for _, provider := range splitList(c.Notification["provider"]) {
		var b []byte
		headers := map[string]string{}
		query := url.Values{}
		switch provider {
		case "ServerChan":
			b, _ = json.Marshal(map[string]string{"text": message, "desp": message})
			headers["Content-Type"] = "application/json"
		case "Qmsg":
			query.Set("msg", message)
		case "Bark":
			query.Set("body", message)
		case "Telegram":
			b = []byte(url.Values{"chat_id": {c.Notification["tg_chat_id"]}, "text": {message}}.Encode())
			headers["Content-Type"] = "application/x-www-form-urlencoded"
		default:
			return errors.New("通知 provider 无效")
		}
		data, e := s.request(ctx, "POST", address, query, b, headers, false)
		if e != nil {
			return e
		}
		if provider == "Telegram" {
			var res map[string]any
			if json.Unmarshal(data, &res) != nil || !truth(res["ok"]) {
				return errors.New("Telegram 未接受通知")
			}
		} else if provider == "ServerChan" {
			var res map[string]any
			if json.Unmarshal(data, &res) != nil {
				return errors.New("Server酱返回非 JSON")
			}
			if code, ok := res["code"]; ok && str(code) != "0" {
				return errors.New("Server酱通知未被接受")
			}
		}
		info("通知已发送：%s", provider)
	}
	return nil
}
func summary(results []TaskResult, total int) string {
	counts := map[Outcome]int{}
	for _, res := range results {
		counts[res.Outcome]++
	}
	notStarted := total - len(results)
	if notStarted < 0 {
		notStarted = 0
	}
	return strings.TrimSpace(fmt.Sprintf("任务处理结束：完成 %d，待确认/仅保存 %d，跳过 %d，失败 %d，未执行 %d", counts[Completed], counts[Pending], counts[Skipped], counts[Failed], notStarted))
}
