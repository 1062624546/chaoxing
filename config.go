package main

import (
	"bufio"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Common, Tiku, Notification map[string]string
	Path, CookiePath           string
	Jobs, MaxAttempts          int
	Speed                      float64
	RetryInterval              time.Duration
}

func parseINI(data string) (map[string]map[string]string, error) {
	sections := map[string]map[string]string{}
	section := ""
	scanner := bufio.NewScanner(strings.NewReader(strings.TrimPrefix(data, "\ufeff")))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	line := 0
	for scanner.Scan() {
		line++
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, ";") || strings.HasPrefix(text, "#") {
			continue
		}
		if strings.HasPrefix(text, "[") && strings.HasSuffix(text, "]") {
			section = strings.ToLower(strings.TrimSpace(text[1 : len(text)-1]))
			if sections[section] == nil {
				sections[section] = map[string]string{}
			}
			continue
		}
		key, value, ok := strings.Cut(text, "=")
		if !ok || section == "" {
			return nil, fmt.Errorf("配置第%d行格式错误", line)
		}
		key = strings.ToLower(strings.TrimSpace(key))
		if key == "" {
			return nil, fmt.Errorf("配置第%d行缺少键名", line)
		}
		if _, exists := sections[section][key]; exists {
			return nil, fmt.Errorf("配置第%d行重复键名：%s", line, key)
		}
		sections[section][key] = strings.TrimSpace(value)
	}
	return sections, scanner.Err()
}
func loadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, errors.New("配置文件无法读取")
	}
	sections, err := parseINI(string(data))
	if err != nil {
		return Config{}, err
	}
	c := Config{Common: sections["common"], Tiku: sections["tiku"], Notification: sections["notification"], Path: path, Jobs: 4, MaxAttempts: 5, Speed: 1, RetryInterval: time.Second}
	if c.Common == nil {
		return c, errors.New("配置缺少 [common]")
	}
	c.CookiePath = os.Getenv("CHAOXING_COOKIES_PATH")
	if c.CookiePath == "" {
		c.CookiePath = filepath.Join(filepath.Dir(path), "cookies.txt")
	}
	if p := c.Common["cookies_path"]; p != "" {
		if !filepath.IsAbs(p) {
			p = filepath.Join(filepath.Dir(path), p)
		}
		c.CookiePath = p
	}
	for key, dst := range map[string]*int{"jobs": &c.Jobs, "max_attempts": &c.MaxAttempts} {
		if s := c.Common[key]; s != "" {
			*dst, err = strconv.Atoi(s)
			if err != nil || *dst < 1 || *dst > 64 {
				return c, fmt.Errorf("%s 应为1至64的整数", key)
			}
		}
	}
	if s := c.Common["speed"]; s != "" {
		c.Speed, err = strconv.ParseFloat(s, 64)
		if err != nil || math.IsNaN(c.Speed) || math.IsInf(c.Speed, 0) || c.Speed < 1 || c.Speed > 2 {
			return c, errors.New("speed 应在1至2之间")
		}
	}
	if s := c.Common["retry_interval"]; s != "" {
		n, e := strconv.ParseFloat(s, 64)
		if e != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 || n > 3600 {
			return c, errors.New("retry_interval 无效")
		}
		c.RetryInterval = time.Duration(n * float64(time.Second))
	}
	for _, m := range []map[string]string{c.Common, c.Tiku} {
		for _, key := range []string{"use_cookies", "work_redo_enabled", "add_learning_count", "submit", "check_llm_connection"} {
			if s := m[key]; s != "" {
				switch strings.ToLower(s) {
				case "true", "false", "1", "0", "yes", "no", "on", "off":
				default:
					return c, fmt.Errorf("%s 布尔值无效", key)
				}
			}
		}
	}
	action := c.Common["notopen_action"]
	if action != "" && action != "continue" && action != "retry" {
		return c, errors.New("notopen_action 仅支持 retry 或 continue")
	}
	for key, spec := range map[string][2]float64{"cover_rate": {0, 1}, "delay": {0, 3600}, "min_interval_seconds": {0, 3600}, "go_min_interval": {0, 3600}, "go_retry_times": {1, 10}, "go_retry_backoff": {0, 60}, "likeapi_retry_times": {1, 10}} {
		if value := c.Tiku[key]; value != "" {
			n, e := strconv.ParseFloat(value, 64)
			if e != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < spec[0] || n > spec[1] {
				return c, fmt.Errorf("%s 数值无效", key)
			}
		}
	}
	return c, nil
}
func yes(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "1", "yes", "on":
		return true
	}
	return false
}
func splitList(s string) []string {
	out := []string{}
	for _, v := range strings.Split(s, ",") {
		v = strings.TrimSpace(v)
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}
func number(s string, def float64) float64 {
	if s == "" {
		return def
	}
	v, e := strconv.ParseFloat(s, 64)
	if e != nil {
		return def
	}
	return v
}
func delay(m map[string]string, key string, def float64) time.Duration {
	return time.Duration(number(m[key], def) * float64(time.Second))
}
