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

func UpdateTelegramMessage(botToken string, chatID any, messageID int, text string) {
	if botToken == "" || chatID == nil || messageID == 0 {
		return
	}

	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/editMessageText", botToken)
	form := url.Values{}
	form.Set("chat_id", fmt.Sprintf("%v", chatID))
	form.Set("message_id", strconv.Itoa(messageID))
	form.Set("text", text)
	form.Set("parse_mode", "HTML")

	client := &http.Client{Timeout: 4 * time.Second}
	_, _ = client.PostForm(apiURL, form)
}

func UpdateProgress(payload TaskPayload, formatLabel string, percent int, statusText string) {
	filled := percent / 10
	bar := strings.Repeat("█", filled) + strings.Repeat("░", 10-filled)

	msg := fmt.Sprintf("⚡ <b>%s (%s)</b>\n\n📊 <code>[%s] %d%%</code>\n🚀 <i>لطفاً کمی شکیبا باشید...</i>",
		statusText, formatLabel, bar, percent)

	UpdateTelegramMessage(payload.BotToken, payload.ChatID, payload.StatusMessageID, msg)

	if payload.MasterCallbackURL != "" {
		callbackData := map[string]any{
			"action":            "progress",
			"step":              "downloading",
			"secret":            payload.Secret,
			"chat_id":           payload.ChatID,
			"status_message_id": payload.StatusMessageID,
			"format_label":      formatLabel,
			"percent":           percent,
		}
		SendMasterCallback(payload.MasterCallbackURL, callbackData)
	}
}

func SendMasterCallback(callbackURL string, data map[string]any) {
	if callbackURL == "" {
		return
	}
	body, _ := json.Marshal(data)
	client := &http.Client{Timeout: 5 * time.Second}
	_, _ = client.Post(callbackURL, "application/json", bytes.NewReader(body))
}

func escapeHTML(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

func UploadToTelegram(payload TaskPayload, filePath, thumbPath, formatLabel, title string, isAudio bool) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer file.Close()

	fi, err := file.Stat()
	if err != nil {
		return "", err
	}
	sizeMB := float64(fi.Size()) / (1024 * 1024)

	safeTitle := escapeHTML(title)
	safeFormat := escapeHTML(formatLabel)

	caption := fmt.Sprintf("✅ <b>دانلود با موفقیت انجام شد</b>\n\n🎬 عنوان: <b>%s</b>\n📁 کیفیت: <code>%s</code>\n📦 حجم فایل: <code>%.2f MB</code>\n\n⚡ <i>ارسال شده توسط ربات دانلود از یوتیوب</i>",
		safeTitle, safeFormat, sizeMB)


	// بررسی سرور محلی تلگرام (پورت 8081 برای آپلودهای تا ۲ گیگابایت)
	apiBase := "http://127.0.0.1:8081"
	testClient := &http.Client{Timeout: 400 * time.Millisecond}
	resp, err := testClient.Get(apiBase)
	if err != nil || (resp != nil && resp.StatusCode >= 500) {
		apiBase = "https://api.telegram.org"
	}
	if resp != nil {
		resp.Body.Close()
	}

	method := "sendVideo"
	field := "video"
	if isAudio {
		method = "sendAudio"
		field = "audio"
	}

	urlEndpoint := fmt.Sprintf("%s/bot%s/%s", apiBase, payload.BotToken, method)

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	_ = writer.WriteField("chat_id", fmt.Sprintf("%v", payload.ChatID))
	_ = writer.WriteField("caption", caption)
	_ = writer.WriteField("parse_mode", "HTML")
	if isAudio {
		_ = writer.WriteField("title", title)
		_ = writer.WriteField("performer", "YouTube Audio")
	} else {
		_ = writer.WriteField("supports_streaming", "true")
	}


	// File
	part, err := writer.CreateFormFile(field, filepath.Base(filePath))
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(part, file); err != nil {
		return "", err
	}

	// Thumbnail
	if thumbPath != "" {
		if thumbFile, err := os.Open(thumbPath); err == nil {
			if thumbPart, err := writer.CreateFormFile("thumbnail", filepath.Base(thumbPath)); err == nil {
				_, _ = io.Copy(thumbPart, thumbFile)
			}
			thumbFile.Close()
		}
	}

	writer.Close()

	req, err := http.NewRequest("POST", urlEndpoint, body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	uploadClient := &http.Client{Timeout: 30 * time.Minute}
	upResp, err := uploadClient.Do(req)
	if err != nil {
		return "", err
	}
	defer upResp.Body.Close()

	var resData struct {
		OK     bool `json:"ok"`
		Result struct {
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
	_ = json.NewDecoder(upResp.Body).Decode(&resData)

	if upResp.StatusCode >= 200 && upResp.StatusCode < 300 {
		fileID := resData.Result.Video.FileID
		if fileID == "" {
			fileID = resData.Result.Audio.FileID
		}
		if fileID == "" {
			fileID = resData.Result.Document.FileID
		}
		return fileID, nil
	}
	return "", fmt.Errorf("upload returned status %d", upResp.StatusCode)
}

