package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var (
	activeTasksMu sync.Mutex
	activeTasks   = make(map[string]TaskPayload)
)

func getDownloadDir() string {
	if fi, err := os.Stat("/dev/shm"); err == nil && fi.IsDir() {
		return "/dev/shm/downloads"
	}
	return "./storage/downloads"
}

func processTask(payload TaskPayload) {
	activeTasksMu.Lock()
	activeTasks[payload.TaskToken] = payload
	activeTasksMu.Unlock()

	defer func() {
		activeTasksMu.Lock()
		delete(activeTasks, payload.TaskToken)
		activeTasksMu.Unlock()
	}()

	downloadDir := getDownloadDir()
	_ = os.MkdirAll(downloadDir, 0777)

	cleanToken := payload.TaskToken
	formatLabel := payload.Quality

	Logger.Log(payload.TaskToken, "START", fmt.Sprintf("Processing %s quality: %s", payload.VideoURL, payload.Quality))

	// Step 1: 15% progress
	UpdateProgress(payload, formatLabel, 15, "در حال استخراج لینک‌های دانلود")

	extracted, err := ExtractFromRapidAPI(payload.VideoURL, payload.Quality)
	if err != nil {
		Logger.Log(payload.TaskToken, "ERROR", "Extraction failed: "+err.Error())
		UpdateTelegramMessage(payload.BotToken, payload.ChatID, payload.StatusMessageID,
			"❌ <b>خطا در دریافت لینک‌های مستقیم از یوتیوب:</b>\n"+err.Error())
		if payload.MasterCallbackURL != "" {
			SendMasterCallback(payload.MasterCallbackURL, map[string]any{
				"action":            "error",
				"secret":            payload.Secret,
				"chat_id":           payload.ChatID,
				"status_message_id": payload.StatusMessageID,
				"error":             err.Error(),
			})
		}
		return
	}

	// Step 2: 50% progress
	UpdateProgress(payload, formatLabel, 50, "در حال دانلود پرسرعت استریم")

	defer func() {
		files, _ := filepath.Glob(filepath.Join(downloadDir, cleanToken+"*"))
		for _, f := range files {
			_ = os.Remove(f)
		}
	}()

	var downloadedFile string

	if extracted.Type == "audio" {
		audioPath := filepath.Join(downloadDir, fmt.Sprintf("%s.mp3", cleanToken))
		if err := DownloadStream(extracted.AudioURL, audioPath, 10*time.Minute); err != nil {
			Logger.Log(payload.TaskToken, "ERROR", "Audio download failed: "+err.Error())
			UpdateTelegramMessage(payload.BotToken, payload.ChatID, payload.StatusMessageID, "❌ خطا در دانلود فایل صوتی: "+err.Error())
			if payload.MasterCallbackURL != "" {
				SendMasterCallback(payload.MasterCallbackURL, map[string]any{
					"action":            "error",
					"secret":            payload.Secret,
					"chat_id":           payload.ChatID,
					"status_message_id": payload.StatusMessageID,
					"error":             err.Error(),
				})
			}
			return
		}
		downloadedFile = audioPath
	} else {
		videoPath := filepath.Join(downloadDir, fmt.Sprintf("%s_v.mp4", cleanToken))
		if err := DownloadStream(extracted.VideoURL, videoPath, 15*time.Minute); err != nil {
			Logger.Log(payload.TaskToken, "ERROR", "Video download failed: "+err.Error())
			UpdateTelegramMessage(payload.BotToken, payload.ChatID, payload.StatusMessageID, "❌ خطا در دانلود فایل ویدیو: "+err.Error())
			if payload.MasterCallbackURL != "" {
				SendMasterCallback(payload.MasterCallbackURL, map[string]any{
					"action":            "error",
					"secret":            payload.Secret,
					"chat_id":           payload.ChatID,
					"status_message_id": payload.StatusMessageID,
					"error":             err.Error(),
				})
			}
			return
		}

		if extracted.AudioURL != "" && !extracted.HasAudio {
			audioPath := filepath.Join(downloadDir, fmt.Sprintf("%s_a.m4a", cleanToken))
			if err := DownloadStream(extracted.AudioURL, audioPath, 10*time.Minute); err == nil {
				mergedPath := filepath.Join(downloadDir, fmt.Sprintf("%s.mp4", cleanToken))
				if err := MergeVideoAudio(videoPath, audioPath, mergedPath); err == nil {
					_ = os.Remove(videoPath)
					_ = os.Remove(audioPath)
					downloadedFile = mergedPath
				} else {
					downloadedFile = videoPath
				}
			} else {
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
		if err := GenerateThumbnail(downloadedFile, tPath); err == nil {
			thumbPath = tPath
			defer os.Remove(tPath)
		}
	}

	fileID, err := UploadToTelegram(payload, downloadedFile, thumbPath, formatLabel, extracted.Title, extracted.Type == "audio")
	if err != nil {
		Logger.Log(payload.TaskToken, "ERROR", "Upload failed: "+err.Error())
		UpdateTelegramMessage(payload.BotToken, payload.ChatID, payload.StatusMessageID, "❌ خطا در آپلود فایل در تلگرام: "+err.Error())
		if payload.MasterCallbackURL != "" {
			SendMasterCallback(payload.MasterCallbackURL, map[string]any{
				"action":            "error",
				"secret":            payload.Secret,
				"chat_id":           payload.ChatID,
				"status_message_id": payload.StatusMessageID,
				"error":             err.Error(),
			})
		}
		return
	}

	// Delete status message
	if payload.BotToken != "" && payload.ChatID != nil && payload.StatusMessageID != 0 {
		delURL := fmt.Sprintf("https://api.telegram.org/bot%s/deleteMessage?chat_id=%v&message_id=%d", payload.BotToken, payload.ChatID, payload.StatusMessageID)
		_, _ = http.Get(delURL)
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
		})
	}

	Logger.Log(payload.TaskToken, "FINISH", "Task finished successfully")

}

func handleProcess(w http.ResponseWriter, r *http.Request) {
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

	// Also support PHP style api.php query
	action := r.URL.Query().Get("action")
	if action == "logs" || r.URL.Path == "/logs" || r.URL.Path == "/api/logs" {
		lines := Logger.GetLogs(50)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok",
			"logs":   strings.Join(lines, "\n"),
		})
		return
	}

	if action == "health" || r.URL.Path == "/health" || r.URL.Path == "/api/health" {
		activeTasksMu.Lock()
		count := len(activeTasks)
		activeTasksMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":       "ok",
			"healthy":      true,
			"active_tasks": count,
			"engine":       "Go 1.22 Native",
		})
		return
	}

	if payload.TaskToken == "" {
		payload.TaskToken = fmt.Sprintf("task_%d", time.Now().UnixNano())
	}

	go processTask(payload)

	w.Header().Set("Content-Type", "application/json")
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
	http.HandleFunc("/logs", handleProcess)
	http.HandleFunc("/api/logs", handleProcess)
	http.HandleFunc("/api/process", handleProcess)
	http.HandleFunc("/api.php", handleProcess)
	http.HandleFunc("/", handleProcess)

	fmt.Printf("🚀 Ultra High-Speed Go Worker running on :%s\n", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		fmt.Printf("Fatal error: %v\n", err)
	}
}
