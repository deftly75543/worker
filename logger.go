package main

import (
	"fmt"
	"sync"
	"time"
)

type MemoryLogger struct {
	mu   sync.RWMutex
	logs []string
	max  int
}

var Logger = &MemoryLogger{
	logs: make([]string, 0, 100),
	max:  100,
}

func (l *MemoryLogger) Log(token, tag, message string) {
	entry := fmt.Sprintf("[%s] [%s] [%s] %s", time.Now().Format("2006-01-02 15:04:05"), token, tag, message)
	fmt.Println(entry)

	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.logs) >= l.max {
		l.logs = l.logs[1:]
	}
	l.logs = append(l.logs, entry)
}

func (l *MemoryLogger) GetLogs(lines int) []string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if lines <= 0 || lines > len(l.logs) {
		lines = len(l.logs)
	}
	start := len(l.logs) - lines
	if start < 0 {
		start = 0
	}
	res := make([]string, len(l.logs)-start)
	copy(res, l.logs[start:])
	return res
}
