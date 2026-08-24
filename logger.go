package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

type LogLevel int

const (
	DEBUG LogLevel = iota
	INFO
	WARN
	ERROR
	CRITICAL
)

func (l LogLevel) String() string {
	switch l {
	case DEBUG:
		return "DEBUG"
	case INFO:
		return "INFO"
	case WARN:
		return "WARN"
	case ERROR:
		return "ERROR"
	case CRITICAL:
		return "CRITICAL"
	default:
		return "INFO"
	}
}

type LogEntry struct {
	Timestamp time.Time `json:"timestamp"`
	Level     string    `json:"level"`
	Tag       string    `json:"tag"`
	Token     string    `json:"token"`
	Message   string    `json:"message"`
}

type AdvancedLogger struct {
	mu       sync.RWMutex
	entries  []LogEntry
	maxLogs  int
	filePath string
	fileMu   sync.Mutex
}

var Logger = &AdvancedLogger{
	entries: make([]LogEntry, 0, 500),
	maxLogs: 500,
}

func init() {
	logDir := "/app/storage/logs"
	if fi, err := os.Stat(logDir); err == nil && fi.IsDir() {
		Logger.filePath = filepath.Join(logDir, "worker.log")
	} else {
		_ = os.MkdirAll("./storage/logs", 0777)
		Logger.filePath = "./storage/logs/worker.log"
	}
}

func (l *AdvancedLogger) log(level LogLevel, tag, token, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	now := time.Now().UTC()

	entry := LogEntry{
		Timestamp: now,
		Level:     level.String(),
		Tag:       tag,
		Token:     token,
		Message:   msg,
	}

	formatted := fmt.Sprintf("[%s] [%-5s] [%-12s] [%s] %s",
		now.Format("2006-01-02 15:04:05.000"),
		level.String(),
		tag,
		token,
		msg,
	)

	// چاپ در لاگ‌های کنسول (stdout) ریل‌وی
	fmt.Println(formatted)

	// ذخیره در بافر چرخان حافظه
	l.mu.Lock()
	if len(l.entries) >= l.maxLogs {
		l.entries = l.entries[1:]
	}
	l.entries = append(l.entries, entry)
	l.mu.Unlock()

	// نوشتن در فایل با چرخش خودکار
	if l.filePath != "" {
		go l.writeToFile(formatted)
	}
}

func (l *AdvancedLogger) writeToFile(line string) {
	l.fileMu.Lock()
	defer l.fileMu.Unlock()

	if fi, err := os.Stat(l.filePath); err == nil && fi.Size() > 10*1024*1024 {
		_ = os.Rename(l.filePath, l.filePath+".old")
	}

	f, err := os.OpenFile(l.filePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0666)
	if err == nil {
		defer f.Close()
		_, _ = f.WriteString(line + "\n")
	}
}

func (l *AdvancedLogger) Debug(tag, token, format string, args ...any) {
	l.log(DEBUG, tag, token, format, args...)
}

func (l *AdvancedLogger) Info(tag, token, format string, args ...any) {
	l.log(INFO, tag, token, format, args...)
}

func (l *AdvancedLogger) Warn(tag, token, format string, args ...any) {
	l.log(WARN, tag, token, format, args...)
}

func (l *AdvancedLogger) Error(tag, token, format string, args ...any) {
	l.log(ERROR, tag, token, format, args...)
}

func (l *AdvancedLogger) Critical(tag, token, format string, args ...any) {
	l.log(CRITICAL, tag, token, format, args...)
}

func (l *AdvancedLogger) Log(token, tag, message string) {
	level := INFO
	tUpper := strings.ToUpper(tag)
	if strings.Contains(tUpper, "ERR") {
		level = ERROR
	} else if strings.Contains(tUpper, "WARN") {
		level = WARN
	}
	l.log(level, tag, token, "%s", message)
}

func (l *AdvancedLogger) GetLogs(lines int, levelFilter, tokenFilter string) []string {
	l.mu.RLock()
	defer l.mu.RUnlock()

	var filtered []string
	levelFilter = strings.ToUpper(strings.TrimSpace(levelFilter))
	tokenFilter = strings.TrimSpace(tokenFilter)

	for _, e := range l.entries {
		if levelFilter != "" && e.Level != levelFilter {
			continue
		}
		if tokenFilter != "" && !strings.Contains(e.Token, tokenFilter) {
			continue
		}
		str := fmt.Sprintf("[%s] [%-5s] [%-10s] [%s] %s",
			e.Timestamp.Format("15:04:05.000"),
			e.Level,
			e.Tag,
			e.Token,
			e.Message,
		)
		filtered = append(filtered, str)
	}

	if lines <= 0 || lines > len(filtered) {
		lines = len(filtered)
	}
	start := len(filtered) - lines
	if start < 0 {
		start = 0
	}
	return filtered[start:]
}

func (l *AdvancedLogger) GetDiagnostics(downloadDir string, activeTasksCount int) map[string]any {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	freeMB, totalMB, _ := getDiskSpaceMB(downloadDir)

	return map[string]any{
		"system": map[string]any{
			"go_version": runtime.Version(),
			"num_cpu":    runtime.NumCPU(),
			"goroutines": runtime.NumGoroutine(),
		},
		"memory_mb": map[string]any{
			"alloc":      mem.Alloc / (1024 * 1024),
			"total_sys":  mem.Sys / (1024 * 1024),
			"heap_alloc": mem.HeapAlloc / (1024 * 1024),
		},
		"storage": map[string]any{
			"download_dir": downloadDir,
			"free_mb":      freeMB,
			"total_mb":     totalMB,
			"used_percent": func() float64 {
				if totalMB == 0 {
					return 0
				}
				return float64(totalMB-freeMB) / float64(totalMB) * 100
			}(),
		},
		"tasks": map[string]any{
			"active_count": activeTasksCount,
		},
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	}
}
