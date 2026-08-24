package main

import (
	"context"
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
	taskCancels   sync.Map
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

	ctx, cancel := context.WithCancel(context.Background())
	taskCancels.Store(token, cancel)

	activeTasksMu.Lock()
	activeTasks[token] = payload
	activeTasksMu.Unlock()

	defer func() {
		taskCancels.Delete(token)
		cancel()
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

	extracted, err := ExtractFromRapidAPI(payload.VideoURL, payload.Quality, payload.AudioLang, token)
	if err != nil {
		if ctx.Err() != nil {
			Logger.Info("TASK", token, "Task was cancelled during extraction")
			return
		}
		Logger.Error("TASK", token, "Extraction FAILED: %v", err)
		UpdateTelegramMessage(payload.BotToken, payload.ChatID, payload.StatusMessageID,
			"❌ <b>خطا در دریافت لینک‌های مستقیم از یوتیوب:</b>\n"+err.Error(), token)
		if payload.MasterCallbackURL != "" {
			SendMasterCallback(payload.MasterCallbackURL, map[string]any{
				"action":            "error",
				"secret":            payload.Secret,
				"chat_id":           FormatChatID(payload.ChatID),
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
		onAudioProgress := func(written, total int64, speedMBs float64, percent int) {
			mappedPercent := 20 + int(float64(percent)*0.70)
			UpdateLiveDownloadProgress(payload, formatLabel, mappedPercent, written, total, speedMBs, "در حال دانلود فایل صوتی MP3")
		}

		if err := DownloadStream(ctx, extracted.AudioURL, audioPath, 15*time.Minute, token, "Audio Stream", onAudioProgress); err != nil {
			if ctx.Err() != nil {
				Logger.Info("TASK", token, "Audio download cancelled by user")
				return
			}
			Logger.Error("TASK", token, "Audio stream download FAILED: %v", err)
			UpdateTelegramMessage(payload.BotToken, payload.ChatID, payload.StatusMessageID, "❌ خطا در دانلود فایل صوتی: "+err.Error(), token)
			if payload.MasterCallbackURL != "" {
				SendMasterCallback(payload.MasterCallbackURL, map[string]any{
					"action":            "error",
					"secret":            payload.Secret,
					"chat_id":           FormatChatID(payload.ChatID),
					"status_message_id": payload.StatusMessageID,
					"error":             err.Error(),
				}, token)
			}
			return
		}
		downloadedFile = audioPath
	} else {
		videoPath := filepath.Join(downloadDir, fmt.Sprintf("%s_v.mp4", cleanToken))
		audioPath := filepath.Join(downloadDir, fmt.Sprintf("%s_a.m4a", cleanToken))

		var videoErr, audioErr error
		var wgDL sync.WaitGroup

		wgDL.Add(1)
		go func() {
			defer wgDL.Done()
			onVideoProgress := func(written, total int64, speedMBs float64, percent int) {
				mappedPercent := 20 + int(float64(percent)*0.68)
				stageMsg := "در حال دانلود پرسرعت موازی"
				if payload.PrefLang == "en" {
					stageMsg = "Downloading video & audio in parallel"
				}
				UpdateLiveDownloadProgress(payload, formatLabel, mappedPercent, written, total, speedMBs, stageMsg)
			}
			videoErr = DownloadStream(ctx, extracted.VideoURL, videoPath, 25*time.Minute, token, "Video Stream", onVideoProgress)
		}()

		if extracted.AudioURL != "" {
			wgDL.Add(1)
			go func() {
				defer wgDL.Done()
				audioErr = DownloadStream(ctx, extracted.AudioURL, audioPath, 15*time.Minute, token, "Parallel Audio Stream", nil)
			}()
		}

		wgDL.Wait()

		if videoErr != nil {
			if ctx.Err() != nil {
				Logger.Info("TASK", token, "Video download cancelled by user")
				return
			}
			Logger.Error("TASK", token, "Video stream download FAILED: %v", videoErr)
			UpdateTelegramMessage(payload.BotToken, payload.ChatID, payload.StatusMessageID, "❌ خطا در دانلود فایل ویدیو: "+videoErr.Error(), token)
			if payload.MasterCallbackURL != "" {
				SendMasterCallback(payload.MasterCallbackURL, map[string]any{
					"action":            "error",
					"secret":            payload.Secret,
					"chat_id":           FormatChatID(payload.ChatID),
					"status_message_id": payload.StatusMessageID,
					"error":             videoErr.Error(),
				}, token)
			}
			return
		}

		if extracted.AudioURL != "" && audioErr == nil {
			mergeMsg := "در حال ادغام فوق‌سریع صدا و تصویر"
			if payload.PrefLang == "en" {
				mergeMsg = "Fast merging video and audio tracks"
			}
			UpdateProgress(payload, formatLabel, 90, mergeMsg)
			mergedPath := filepath.Join(downloadDir, fmt.Sprintf("%s.mp4", cleanToken))
			if err := MergeVideoAudio(videoPath, audioPath, mergedPath, token); err == nil {
				_ = os.Remove(videoPath)
				_ = os.Remove(audioPath)
				downloadedFile = mergedPath
			} else {
				Logger.Warn("TASK", token, "FFmpeg merge failed, proceeding with raw video: %v", err)
				downloadedFile = videoPath
			}
		} else {
			downloadedFile = videoPath
		}
	}

	// Step 3: 95% Uploading & Media Transformations
	UpdateProgress(payload, formatLabel, 95, "در حال پردازش و آپلود نهایی در تلگرام")

	var thumbPath string
	tPath := filepath.Join(downloadDir, fmt.Sprintf("%s_thumb.jpg", cleanToken))
	if err := GenerateThumbnail(downloadedFile, tPath, token); err == nil {
		thumbPath = tPath
		defer os.Remove(tPath)
	}

	// ۱. برش بازه زمانی (Trim & Cut)
	if payload.TrimStart != "" && payload.TrimDuration != "" {
		ext := filepath.Ext(downloadedFile)
		trimmedPath := filepath.Join(downloadDir, fmt.Sprintf("%s_trimmed%s", cleanToken, ext))
		if err := TrimMedia(downloadedFile, trimmedPath, payload.TrimStart, payload.TrimDuration, token); err == nil {
			downloadedFile = trimmedPath
		}
	}

	// ۲. تیزر متحرک (Preview GIF / Animation)
	if payload.SendFormat == "gif" {
		gifPath := filepath.Join(downloadDir, fmt.Sprintf("%s_preview.mp4", cleanToken))
		if err := GeneratePreviewGIF(downloadedFile, gifPath, token); err == nil {
			downloadedFile = gifPath
		}
	}

	// ۳. فشرده‌سازی کاهش مصرف داده (Data Saver)
	if payload.DataSaver && extracted.Type == "video" && payload.SendFormat != "gif" {
		compPath := filepath.Join(downloadDir, fmt.Sprintf("%s_compressed.mp4", cleanToken))
		if err := CompressVideo(downloadedFile, compPath, token); err == nil {
			downloadedFile = compPath
		}
	}

	// ۴. تبدیل به ویس مسیج (Telegram Voice OGG)
	if payload.SendFormat == "voice" {
		voicePath := filepath.Join(downloadDir, fmt.Sprintf("%s.ogg", cleanToken))
		if err := ConvertToVoiceOGG(downloadedFile, voicePath, token); err == nil {
			downloadedFile = voicePath
		}
	}

	// ۵. ساخت رینگتون ۳۰ ثانیه‌ای (Ringtone Maker)
	if payload.SendFormat == "ringtone" || payload.SendFormat == "ring" {
		ringPath := filepath.Join(downloadDir, fmt.Sprintf("%s_ring.mp3", cleanToken))
		if err := MakeRingtone(downloadedFile, ringPath, token); err == nil {
			downloadedFile = ringPath
		}
	}

	// ۶. متادیتا و کاور آرت برای فایل MP3
	if extracted.Type == "audio" && payload.SendFormat != "voice" && payload.SendFormat != "ringtone" && payload.SendFormat != "ring" {
		taggedPath := filepath.Join(downloadDir, fmt.Sprintf("%s_tagged.mp3", cleanToken))
		if err := EmbedID3Tags(downloadedFile, taggedPath, extracted.Title, "YouTube Music", thumbPath, token); err == nil {
			downloadedFile = taggedPath
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
				"chat_id":           FormatChatID(payload.ChatID),
				"status_message_id": payload.StatusMessageID,
				"error":             err.Error(),
			}, token)
		}
		return
	}

	// بلافاصله پس از اتمام آپلود، تمامی فایل‌های محلی از دیسک پاک می‌شوند (Zero-Disk Retention)
	if downloadedFile != "" {
		_ = os.Remove(downloadedFile)
	}
	allTokenFiles, _ := filepath.Glob(filepath.Join(downloadDir, cleanToken+"*"))
	for _, f := range allTokenFiles {
		_ = os.Remove(f)
	}
	Logger.Info("TASK", token, "Zero-Disk Retention: Purged all temporary media files from disk for token %s", cleanToken)

	// Delete status message
	targetChatID := FormatChatID(payload.ChatID)
	if payload.BotToken != "" && targetChatID != "" && payload.StatusMessageID != 0 {
		delURL := fmt.Sprintf("https://api.telegram.org/bot%s/deleteMessage?chat_id=%s&message_id=%d", payload.BotToken, targetChatID, payload.StatusMessageID)
		delResp, delErr := http.Get(delURL)
		if delErr == nil && delResp != nil {
			delResp.Body.Close()
		}
		Logger.Debug("TASK", token, "Requested deletion of status message %d in chat %s", payload.StatusMessageID, targetChatID)
	}

	// Complete callback to Master
	if payload.MasterCallbackURL != "" {
		SendMasterCallback(payload.MasterCallbackURL, map[string]any{
			"action":            "complete",
			"secret":            payload.Secret,
			"chat_id":           targetChatID,
			"status_message_id": payload.StatusMessageID,
			"video_id":          payload.VideoID,
			"quality":           payload.Quality,
			"audio_lang":        payload.AudioLang,
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

	// Endpoint: /cancel یا /api/cancel
	if action == "cancel" || path == "/cancel" || path == "/api/cancel" {
		tokenToCancel := r.URL.Query().Get("token")
		if tokenToCancel == "" {
			var cancelBody struct {
				TaskToken string `json:"task_token"`
			}
			_ = json.NewDecoder(r.Body).Decode(&cancelBody)
			tokenToCancel = cancelBody.TaskToken
		}
		cancelled := false
		if tokenToCancel != "" {
			if val, ok := taskCancels.Load(tokenToCancel); ok {
				if cancelFunc, isFn := val.(context.CancelFunc); isFn {
					cancelFunc()
					cancelled = true
					Logger.Info("TASK", tokenToCancel, "Task CANCELLED by user request via API")
				}
			}
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":     "ok",
			"cancelled":  cancelled,
			"task_token": tokenToCancel,
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
			payload.AudioLang = r.FormValue("audio_lang")
			payload.SendFormat = r.FormValue("send_format")
			payload.TrimStart = r.FormValue("trim_start")
			payload.TrimDuration = r.FormValue("trim_duration")
			payload.ProgressTheme = r.FormValue("progress_theme")
			if r.FormValue("data_saver") == "1" || r.FormValue("data_saver") == "true" {
				payload.DataSaver = true
			}
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
	http.HandleFunc("/cancel", handleProcess)
	http.HandleFunc("/api/cancel", handleProcess)
	http.HandleFunc("/api/process", handleProcess)
	http.HandleFunc("/api.php", handleProcess)
	http.HandleFunc("/", handleProcess)

	Logger.Info("SERVER", "SYSTEM", "🚀 Advanced Go Worker started on :%s (Version: 2.2, Go: %s)", port, startTime.Format(time.RFC3339))

	// موتور پاکسازی خودکار دیسک (Zero-Disk Retention Scrubber)
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			downloadDir := getDownloadDir()
			files, _ := filepath.Glob(filepath.Join(downloadDir, "*"))
			for _, f := range files {
				if fi, err := os.Stat(f); err == nil && time.Since(fi.ModTime()) > 60*time.Second {
					_ = os.Remove(f)
					Logger.Debug("SCRUBBER", "SYSTEM", "Zero-Disk Retention: Purged stale temporary file %s", f)
				}
			}
		}
	}()

	if err := http.ListenAndServe(":"+port, nil); err != nil {
		Logger.Critical("SERVER", "SYSTEM", "Fatal server listener error: %v", err)
	}
}
