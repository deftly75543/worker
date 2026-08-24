package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type TaskPayload struct {
	TaskToken         string `json:"task_token"`
	VideoURL          string `json:"video_url"`
	VideoID           string `json:"video_id"`
	Quality           string `json:"quality"`
	IsAudio           bool   `json:"is_audio"`
	ChatID            any    `json:"chat_id"`
	StatusMessageID   int    `json:"status_message_id"`
	BotToken          string `json:"bot_token"`
	MasterCallbackURL string `json:"master_callback_url"`
	Secret            string `json:"secret"`
}

var (
	lastMsgEditMu sync.Mutex
	lastMsgEdit   = make(map[string]time.Time)
)

func FormatChatID(chatID any) string {
	if chatID == nil {
		return ""
	}
	switch v := chatID.(type) {
	case string:
		return strings.TrimSpace(v)
	case float64:
		return strconv.FormatInt(int64(v), 10)
	case float32:
		return strconv.FormatInt(int64(v), 10)
	case int64:
		return strconv.FormatInt(v, 10)
	case int:
		return strconv.Itoa(v)
	case json.Number:
		return v.String()
	default:
		s := fmt.Sprintf("%v", v)
		if strings.Contains(s, "e+") || strings.Contains(s, "E+") {
			if f, err := strconv.ParseFloat(s, 64); err == nil {
				return strconv.FormatInt(int64(f), 10)
			}
		}
		return s
	}
}

func UpdateTelegramMessage(botToken string, chatID any, messageID int, text, token string) {
	targetChatID := FormatChatID(chatID)
	if botToken == "" || targetChatID == "" || messageID == 0 {
		return
	}

	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/editMessageText", botToken)
	form := url.Values{}
	form.Set("chat_id", targetChatID)
	form.Set("message_id", strconv.Itoa(messageID))
	form.Set("text", text)
	form.Set("parse_mode", "HTML")

	if token != "" {
		replyMarkup := fmt.Sprintf(`{"inline_keyboard":[[{"text":"❌ لغو دانلود","callback_data":"cancel:%s"}]]}`, token)
		form.Set("reply_markup", replyMarkup)
	}

	client := &http.Client{Timeout: 6 * time.Second}
	resp, err := client.PostForm(apiURL, form)
	if err != nil {
		Logger.Warn("TELEGRAM", token, "Failed to edit message %d (Chat: %s): %v", messageID, targetChatID, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		Logger.Debug("TELEGRAM", token, "Edit message returned HTTP %d: %s", resp.StatusCode, string(body))
	}
}

func UpdateProgress(payload TaskPayload, formatLabel string, percent int, statusText string) {
	filled := percent / 10
	if filled > 10 {
		filled = 10
	}
	bar := strings.Repeat("█", filled) + strings.Repeat("░", 10-filled)

	msg := fmt.Sprintf("⚡ <b>%s (%s)</b>\n\n📊 <code>[%s] %d%%</code>\n🚀 <i>لطفاً کمی شکیبا باشید...</i>",
		statusText, formatLabel, bar, percent)

	UpdateTelegramMessage(payload.BotToken, payload.ChatID, payload.StatusMessageID, msg, payload.TaskToken)

	if payload.MasterCallbackURL != "" {
		callbackData := map[string]any{
			"action":            "progress",
			"step":              "processing",
			"secret":            payload.Secret,
			"chat_id":           FormatChatID(payload.ChatID),
			"status_message_id": payload.StatusMessageID,
			"format_label":      formatLabel,
			"percent":           percent,
		}
		SendMasterCallback(payload.MasterCallbackURL, callbackData, payload.TaskToken)
	}
}

func UpdateLiveDownloadProgress(payload TaskPayload, formatLabel string, stagePercent int, writtenBytes, totalBytes int64, speedMBs float64, stageText string) {
	token := payload.TaskToken

	// کنترل نرخ درخواست جهت احترام به Rate Limit تلگرام (حداقل ۲ ثانیه بین هر ادیت)
	lastMsgEditMu.Lock()
	lastTime, exists := lastMsgEdit[token]
	if exists && time.Since(lastTime) < 2200*time.Millisecond && stagePercent < 100 {
		lastMsgEditMu.Unlock()
		return
	}
	lastMsgEdit[token] = time.Now()
	lastMsgEditMu.Unlock()

	filled := stagePercent / 10
	if filled > 10 {
		filled = 10
	}
	bar := strings.Repeat("█", filled) + strings.Repeat("░", 10-filled)

	var sizeDetails string
	if totalBytes > 0 {
		curMB := float64(writtenBytes) / (1024 * 1024)
		totMB := float64(totalBytes) / (1024 * 1024)
		sizeDetails = fmt.Sprintf("📥 <b>دانلود شده:</b> <code>%.1f MB / %.1f MB</code>\n⚡ <b>سرعت دانلود:</b> <code>%.2f MB/s</code>", curMB, totMB, speedMBs)
	} else if writtenBytes > 0 {
		curMB := float64(writtenBytes) / (1024 * 1024)
		sizeDetails = fmt.Sprintf("📥 <b>دانلود شده:</b> <code>%.1f MB</code>\n⚡ <b>سرعت دانلود:</b> <code>%.2f MB/s</code>", curMB, speedMBs)
	}

	msg := fmt.Sprintf("⚡ <b>%s (%s)</b>\n\n📊 <code>[%s] %d%%</code>\n%s\n\n🚀 <i>لطفاً کمی شکیبا باشید...</i>",
		stageText, formatLabel, bar, stagePercent, sizeDetails)

	UpdateTelegramMessage(payload.BotToken, payload.ChatID, payload.StatusMessageID, msg, token)
}

func SendMasterCallback(callbackURL string, data map[string]any, token string) {
	if callbackURL == "" {
		return
	}
	body, err := json.Marshal(data)
	if err != nil {
		Logger.Error("CALLBACK", token, "Failed to marshal master callback payload: %v", err)
		return
	}

	client := &http.Client{Timeout: 7 * time.Second}
	startTime := time.Now()
	resp, err := client.Post(callbackURL, "application/json", bytes.NewReader(body))
	elapsed := time.Since(startTime)

	action := fmt.Sprintf("%v", data["action"])
	if err != nil {
		Logger.Warn("CALLBACK", token, "Master callback ('%s') failed after %v: %v", action, elapsed, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		Logger.Warn("CALLBACK", token, "Master callback ('%s') returned HTTP %d in %v", action, resp.StatusCode, elapsed)
	} else {
		Logger.Debug("CALLBACK", token, "Master callback ('%s') dispatched successfully in %v", action, elapsed)
	}
}

func escapeHTML(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

func UploadToTelegram(payload TaskPayload, filePath, thumbPath, formatLabel, title string, isAudio bool) (string, error) {
	token := payload.TaskToken
	targetChatID := FormatChatID(payload.ChatID)
	startTime := time.Now()

	file, err := os.Open(filePath)
	if err != nil {
		Logger.Error("UPLOADER", token, "Failed to open file for upload %s: %v", filePath, err)
		return "", err
	}
	defer file.Close()

	fi, err := file.Stat()
	if err != nil {
		Logger.Error("UPLOADER", token, "Failed to stat file %s: %v", filePath, err)
		return "", err
	}
	sizeMB := float64(fi.Size()) / (1024 * 1024)

	safeTitle := escapeHTML(title)
	safeFormat := escapeHTML(formatLabel)

	caption := fmt.Sprintf("✅ <b>دانلود با موفقیت انجام شد</b>\n\n🎬 عنوان: <b>%s</b>\n📁 کیفیت: <code>%s</code>\n📦 حجم فایل: <code>%.2f MB</code>\n\n⚡ <i>ارسال شده توسط ربات دانلود از یوتیوب</i>",
		safeTitle, safeFormat, sizeMB)

	// بررسی سرور محلی تلگرام (پورت 8081 برای آپلودهای تا ۲ گیگابایت)
	apiBase := "http://127.0.0.1:8081"
	testClient := &http.Client{Timeout: 500 * time.Millisecond}
	resp, testErr := testClient.Get(apiBase)
	isLocal := false
	if testErr == nil && resp != nil && resp.StatusCode < 500 {
		isLocal = true
		resp.Body.Close()
		Logger.Info("UPLOADER", token, "Local Telegram Bot API server detected on :8081 (2GB upload enabled)")
	} else {
		apiBase = "https://api.telegram.org"
		Logger.Warn("UPLOADER", token, "Local Telegram Bot API is unavailable (err: %v). Falling back to official cloud API (50MB cap)", testErr)
	}

	method := "sendVideo"
	field := "video"
	if isAudio {
		method = "sendAudio"
		field = "audio"
	}

	urlEndpoint := fmt.Sprintf("%s/bot%s/%s", apiBase, payload.BotToken, method)
	Logger.Info("UPLOADER", token, "Initiating multipart upload -> %s (Size: %.2f MB, Method: %s, TargetChat: %s, Local: %v)",
		urlEndpoint, sizeMB, method, targetChatID, isLocal)

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	_ = writer.WriteField("chat_id", targetChatID)
	_ = writer.WriteField("caption", caption)
	_ = writer.WriteField("parse_mode", "HTML")
	if isAudio {
		_ = writer.WriteField("title", title)
		_ = writer.WriteField("performer", "YouTube Audio")
	} else {
		_ = writer.WriteField("supports_streaming", "true")
	}

	// File stream
	part, err := writer.CreateFormFile(field, filepath.Base(filePath))
	if err != nil {
		Logger.Error("UPLOADER", token, "Failed to create form file field '%s': %v", field, err)
		return "", err
	}
	if _, err := io.Copy(part, file); err != nil {
		Logger.Error("UPLOADER", token, "Failed to copy file data to multipart buffer: %v", err)
		return "", err
	}

	// Thumbnail
	if thumbPath != "" {
		if thumbFile, err := os.Open(thumbPath); err == nil {
			if thumbPart, err := writer.CreateFormFile("thumbnail", filepath.Base(thumbPath)); err == nil {
				_, _ = io.Copy(thumbPart, thumbFile)
				Logger.Debug("UPLOADER", token, "Attached thumbnail to video upload: %s", thumbPath)
			}
			thumbFile.Close()
		}
	}

	if err := writer.Close(); err != nil {
		Logger.Error("UPLOADER", token, "Failed to finalize multipart writer: %v", err)
		return "", err
	}

	req, err := http.NewRequest("POST", urlEndpoint, body)
	if err != nil {
		Logger.Error("UPLOADER", token, "Failed to create POST request: %v", err)
		return "", err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	uploadClient := &http.Client{Timeout: 35 * time.Minute}
	upResp, err := uploadClient.Do(req)
	uploadElapsed := time.Since(startTime)

	if err != nil {
		Logger.Error("UPLOADER", token, "Upload HTTP request failed after %v: %v", uploadElapsed, err)
		return "", err
	}
	defer upResp.Body.Close()

	respRaw, _ := io.ReadAll(upResp.Body)

	var resData struct {
		OK          bool   `json:"ok"`
		ErrorCode   int    `json:"error_code"`
		Description string `json:"description"`
		Result      struct {
			Video struct {
				FileID string `json:"file_id"`
			} `json:"video"`
			Audio struct {
				FileID string `json:"file_id"`
			} `json:"audio"`
			Document struct {
				FileID string `json:"file_id"`
			} `json:"document"`
		} `json:"result"`
	}
	_ = json.Unmarshal(respRaw, &resData)

	if upResp.StatusCode >= 200 && upResp.StatusCode < 300 && resData.OK {
		fileID := resData.Result.Video.FileID
		if fileID == "" {
			fileID = resData.Result.Audio.FileID
		}
		if fileID == "" {
			fileID = resData.Result.Document.FileID
		}

		speedMBs := sizeMB / uploadElapsed.Seconds()
		Logger.Info("UPLOADER", token, "Upload SUCCESS in %v (avg: %.2f MB/s). FileID: %s",
			uploadElapsed.Round(time.Millisecond), speedMBs, fileID)
		return fileID, nil
	}

	errorMsg := fmt.Sprintf("Telegram upload failed (HTTP %d, Code: %d, Description: %s)",
		upResp.StatusCode, resData.ErrorCode, resData.Description)
	if resData.Description == "" {
		errorMsg = fmt.Sprintf("Telegram upload failed (HTTP %d): %s", upResp.StatusCode, string(respRaw))
	}
	Logger.Error("UPLOADER", token, "%s", errorMsg)
	return "", fmt.Errorf("%s", errorMsg)
}

