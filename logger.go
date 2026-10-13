package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"regexp"
	"strings"
	"sync"
)

var urlLogRE = regexp.MustCompile(`https?://[^\s<>"']+`)
var secretLogRE = regexp.MustCompile(`(?i)(password|passwd|cookie|authorization|token|dtoken|enc|api_key|ticket)(\s*[:=]\s*)[^\s,;]+`)

func redact(s string) string {
	s = urlLogRE.ReplaceAllString(s, "[URL已隐藏]")
	return secretLogRE.ReplaceAllString(s, "$1$2[已隐藏]")
}
func info(format string, args ...any) { log.Print(redact(fmt.Sprintf(format, args...))) }

type rotatingLog struct {
	mu   sync.Mutex
	path string
	f    *os.File
	size int64
}

func newLog(path string) (*rotatingLog, error) {
	f, e := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if e != nil {
		return nil, e
	}
	st, e := f.Stat()
	if e != nil {
		f.Close()
		return nil, e
	}
	return &rotatingLog{path: path, f: f, size: st.Size()}, nil
}
func (w *rotatingLog) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.size+int64(len(p)) > 5<<20 {
		w.f.Close()
		os.Remove(w.path + ".3")
		for i := 2; i >= 1; i-- {
			os.Rename(fmt.Sprintf("%s.%d", w.path, i), fmt.Sprintf("%s.%d", w.path, i+1))
		}
		os.Rename(w.path, w.path+".1")
		f, e := os.OpenFile(w.path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
		if e != nil {
			return 0, e
		}
		w.f = f
		w.size = 0
	}
	n, e := w.f.Write(p)
	w.size += int64(n)
	return n, e
}
func (w *rotatingLog) Close() error { return w.f.Close() }
func setupLog() (io.Closer, error) {
	path := os.Getenv("CHAOXING_LOG_PATH")
	if strings.TrimSpace(path) == "" {
		path = "chaoxing.log"
	}
	w, e := newLog(path)
	if e != nil {
		return nil, e
	}
	log.SetOutput(io.MultiWriter(os.Stderr, w))
	log.SetFlags(log.Ldate | log.Ltime)
	return w, nil
}
