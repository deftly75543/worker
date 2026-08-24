package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	activeTasksMu sync.Mutex
	activeTasks   = make(map[string]TaskPayload)
	startTime     = time.Now()
)

func getDownloadDir() string {
	if fi, err := os.Stat("/app/storage"); err == nil && fi.IsDir() {
		return "/app/storage/downloads"
	}
	return "./storage/downloads"
}

func processTask(payload TaskPayload) {
	token := payload.TaskToken
	taskStart := time.Now()

	activeTasksMu.Lock()
	activeTasks[token] = payload
	activeTasksMu.Unlock()

	defer func() {
		activeTasksMu.Lock()
		delete(activeTasks, token)
		activeTasksMu.Unlock()
	}()

	downloadDir := getDownloadDir()
	_ = os.MkdirAll(downloadDir, 0777)

	cleanToken := token
	formatLabel := payload.Quality

	Logger.Info("TASK", token, "===> Task STARTED. URL: %s | Quality: %s | ChatID: %v | StatusMsgID: %d",
		payload.VideoURL, payload.Quality, payload.ChatID, payload.StatusMessageID)

	// بررسی وضعیت حافظه و دیسک
	freeMB, totalMB, _ := getDiskSpaceMB(downloadDir)
	Logger.Info("STORAGE", token, "Storage at start: %d MB free of %d MB total in %s", freeMB, totalMB, downloadDir)
	if freeMB > 0 && freeMB < 200 {
		Logger.Warn("STORAGE", token, "LOW DISK SPACE WARNING: Only %d MB free. Initiating emergency temp cleanup.", freeMB)
		files, _ := filepath.Glob(filepath.Join(downloadDir, "*"))
		for _, f := range files {
			if fi, err := os.Stat(f); err == nil && time.Since(fi.ModTime()) > 5*time.Minute {
				_ = os.Remove(f)
			}
		}
	}

	// Step 1: 15% progress
	UpdateProgress(payload, formatLabel, 15, "در حال استخراج لینک‌های دانلود")

	extracted, err := ExtractFromRapidAPI(payload.VideoURL, payload.Quality, token)
	if err != nil {
		Logger.Error("TASK", token, "Extraction FAILED: %v", err)
		UpdateTelegramMessage(payload.BotToken, payload.ChatID, payload.StatusMessageID,
			"❌ <b>خطا در دریافت لینک‌های مستقیم از یوتیوب:</b>\n"+err.Error(), token)
		if payload.MasterCallbackURL != "" {
			SendMasterCallback(payload.MasterCallbackURL, map[string]any{
				"action":            "error",
				"secret":            payload.Secret,
				"chat_id":           payload.ChatID,
				"status_message_id": payload.StatusMessageID,
				"error":             err.Error(),
			}, token)
		}
		return
	}

	// Step 2: 50% progress
	UpdateProgress(payload, formatLabel, 50, "در حال دانلود پرسرعت استریم")

	defer func() {
		cleanupFiles, _ := filepath.Glob(filepath.Join(downloadDir, cleanToken+"*"))
		removedCount := 0
		for _, f := range cleanupFiles {
			if err := os.Remove(f); err == nil {
				removedCount++
			}
		}
		Logger.Debug("CLEANUP", token, "Cleaned up %d temporary files for token %s", removedCount, cleanToken)
	}()

	var downloadedFile string

	if extracted.Type == "audio" {
		audioPath := filepath.Join(downloadDir, fmt.Sprintf("%s.mp3", cleanToken))
		if err := DownloadStream(extracted.AudioURL, audioPath, 15*time.Minute, token, "Audio Stream"); err != nil {
			Logger.Error("TASK", token, "Audio stream download FAILED: %v", err)
			UpdateTelegramMessage(payload.BotToken, payload.ChatID, payload.StatusMessageID, "❌ خطا در دانلود فایل صوتی: "+err.Error(), token)
			if payload.MasterCallbackURL != "" {
				SendMasterCallback(payload.MasterCallbackURL, map[string]any{
					"action":            "error",
					"secret":            payload.Secret,
					"chat_id":           payload.ChatID,
					"status_message_id": payload.StatusMessageID,
					"error":             err.Error(),
				}, token)
			}
			return
		}
		downloadedFile = audioPath
	} else {
		videoPath := filepath.Join(downloadDir, fmt.Sprintf("%s_v.mp4", cleanToken))
		if err := DownloadStream(extracted.VideoURL, videoPath, 20*time.Minute, token, "Video Stream"); err != nil {
			Logger.Error("TASK", token, "Video stream download FAILED: %v", err)
			UpdateTelegramMessage(payload.BotToken, payload.ChatID, payload.StatusMessageID, "❌ خطا در دانلود فایل ویدیو: "+err.Error(), token)
			if payload.MasterCallbackURL != "" {
				SendMasterCallback(payload.MasterCallbackURL, map[string]any{
					"action":            "error",
					"secret":            payload.Secret,
					"chat_id":           payload.ChatID,
					"status_message_id": payload.StatusMessageID,
					"error":             err.Error(),
				}, token)
			}
			return
		}

		if extracted.AudioURL != "" && !extracted.HasAudio {
			audioPath := filepath.Join(downloadDir, fmt.Sprintf("%s_a.m4a", cleanToken))
			if err := DownloadStream(extracted.AudioURL, audioPath, 10*time.Minute, token, "DASH Separate Audio"); err == nil {
				mergedPath := filepath.Join(downloadDir, fmt.Sprintf("%s.mp4", cleanToken))
				if err := MergeVideoAudio(videoPath, audioPath, mergedPath, token); err == nil {
					_ = os.Remove(videoPath)
					_ = os.Remove(audioPath)
					downloadedFile = mergedPath
				} else {
					Logger.Warn("TASK", token, "FFmpeg merge failed, proceeding with raw video without merged audio")
					downloadedFile = videoPath
				}
			} else {
				Logger.Warn("TASK", token, "Failed to download separate audio for DASH video: %v", err)
				downloadedFile = videoPath
			}
		} else {
			downloadedFile = videoPath
		}
	}

	// Step 3: 100% Uploading
	UpdateProgress(payload, formatLabel, 100, "در حال آپلود در تلگرام")

	var thumbPath string
	if extracted.Type == "video" {
		tPath := filepath.Join(downloadDir, fmt.Sprintf("%s_thumb.jpg", cleanToken))
		if err := GenerateThumbnail(downloadedFile, tPath, token); err == nil {
			thumbPath = tPath
			defer os.Remove(tPath)
		}
	}

	fileID, err := UploadToTelegram(payload, downloadedFile, thumbPath, formatLabel, extracted.Title, extracted.Type == "audio")
	if err != nil {
		Logger.Error("TASK", token, "Upload to Telegram FAILED: %v", err)
		UpdateTelegramMessage(payload.BotToken, payload.ChatID, payload.StatusMessageID, "❌ خطا در آپلود فایل در تلگرام: "+err.Error(), token)
		if payload.MasterCallbackURL != "" {
			SendMasterCallback(payload.MasterCallbackURL, map[string]any{
				"action":            "error",
				"secret":            payload.Secret,
				"chat_id":           payload.ChatID,
				"status_message_id": payload.StatusMessageID,
				"error":             err.Error(),
			}, token)
		}
		return
	}

	// Delete status message
	if payload.BotToken != "" && payload.ChatID != nil && payload.StatusMessageID != 0 {
		delURL := fmt.Sprintf("https://api.telegram.org/bot%s/deleteMessage?chat_id=%v&message_id=%d", payload.BotToken, payload.ChatID, payload.StatusMessageID)
		delResp, delErr := http.Get(delURL)
		if delErr == nil && delResp != nil {
			delResp.Body.Close()
		}
		Logger.Debug("TASK", token, "Requested deletion of status message %d", payload.StatusMessageID)
	}

	// Complete callback to Master
	if payload.MasterCallbackURL != "" {
		SendMasterCallback(payload.MasterCallbackURL, map[string]any{
			"action":            "complete",
			"secret":            payload.Secret,
			"chat_id":           payload.ChatID,
			"status_message_id": payload.StatusMessageID,
			"video_id":          payload.VideoID,
			"quality":           payload.Quality,
			"is_audio":          payload.IsAudio,
			"telegram_file_id":  fileID,
		}, token)
	}

	totalDuration := time.Since(taskStart)
	Logger.Info("TASK", token, "<=== Task FINISHED SUCCESSFULLY in %v. FileID: %s", totalDuration.Round(time.Millisecond), fileID)
}

func handleProcess(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	action := r.URL.Query().Get("action")

	// Endpoint: /logs یا /api/logs
	if action == "logs" || path == "/logs" || path == "/api/logs" {
		linesLimit := 50
		if lStr := r.URL.Query().Get("lines"); lStr != "" {
			if n, err := strconv.Atoi(lStr); err == nil && n > 0 {
				linesLimit = n
			}
		}
		levelFilter := r.URL.Query().Get("level")
		tokenFilter := r.URL.Query().Get("token")
		format := r.URL.Query().Get("format")

		lines := Logger.GetLogs(linesLimit, levelFilter, tokenFilter)

		if format == "text" {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write([]byte(strings.Join(lines, "\n")))
			return
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":       "ok",
			"total_lines": len(lines),
			"logs":        strings.Join(lines, "\n"),
		})
		return
	}

	// Endpoint: /diag یا /api/diag
	if action == "diag" || path == "/diag" || path == "/api/diag" {
		activeTasksMu.Lock()
		count := len(activeTasks)
		activeTasksMu.Unlock()

		diag := Logger.GetDiagnostics(getDownloadDir(), count)
		diag["uptime_sec"] = int(time.Since(startTime).Seconds())

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(diag)
		return
	}

	// Endpoint: /health یا /api/health
	if action == "health" || path == "/health" || path == "/api/health" {
		activeTasksMu.Lock()
		count := len(activeTasks)
		activeTasksMu.Unlock()

		freeMB, totalMB, _ := getDiskSpaceMB(getDownloadDir())

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":       "ok",
			"healthy":      true,
			"active_tasks": count,
			"disk_free_mb": freeMB,
			"disk_total_mb": totalMB,
			"uptime":       time.Since(startTime).Round(time.Second).String(),
			"engine":       "Go 1.22 Native",
		})
		return
	}

	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var payload TaskPayload
	if r.Method == http.MethodPost {
		if strings.Contains(r.Header.Get("Content-Type"), "application/json") {
			_ = json.NewDecoder(r.Body).Decode(&payload)
		} else {
			_ = r.ParseForm()
			payload.VideoURL = r.FormValue("video_url")
			payload.Quality = r.FormValue("quality")
		}
	}

	if payload.TaskToken == "" {
		payload.TaskToken = fmt.Sprintf("task_%d", time.Now().UnixNano())
	}

	Logger.Info("SERVER", payload.TaskToken, "Received job dispatch from %s for URL: %s", r.RemoteAddr, payload.VideoURL)

	go processTask(payload)

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":     "accepted",
		"task_token": payload.TaskToken,
		"message":    "Task queued in high-speed Go worker",
	})
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	http.HandleFunc("/health", handleProcess)
	http.HandleFunc("/api/health", handleProcess)
	http.HandleFunc("/diag", handleProcess)
	http.HandleFunc("/api/diag", handleProcess)
	http.HandleFunc("/logs", handleProcess)
	http.HandleFunc("/api/logs", handleProcess)
	http.HandleFunc("/api/process", handleProcess)
	http.HandleFunc("/api.php", handleProcess)
	http.HandleFunc("/", handleProcess)

	Logger.Info("SERVER", "SYSTEM", "🚀 Advanced Go Worker started on :%s (Version: 2.1, Go: %s)", port, startTime.Format(time.RFC3339))

	if err := http.ListenAndServe(":"+port, nil); err != nil {
		Logger.Critical("SERVER", "SYSTEM", "Fatal server listener error: %v", err)
	}
}
