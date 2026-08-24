package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type ExtractedMedia struct {
	Type          string // "video" or "audio"
	Title         string
	VideoURL      string
	AudioURL      string
	AudioLangName string
	HasAudio      bool
}

type RapidAPIVideoItem struct {
	Quality  string `json:"quality"`
	FPS      any    `json:"fps"`
	URL      string `json:"url"`
	HasAudio bool   `json:"hasAudio"`
}

type RapidAPIAudioItem struct {
	Quality      string `json:"quality"`
	URL          string `json:"url"`
	Link         string `json:"link"`
	DownloadURL  string `json:"downloadUrl"`
	Language     string `json:"language"`
	LanguageCode string `json:"languageCode"`
	DisplayName  string `json:"displayName"`
	Name         string `json:"name"`
	Label        string `json:"label"`
	IsOriginal   bool   `json:"isOriginal"`
	IsDefault    bool   `json:"isDefault"`
	AudioTrack   struct {
		ID             string `json:"id"`
		DisplayName    string `json:"displayName"`
		Name           string `json:"name"`
		AudioIsDefault bool   `json:"audioIsDefault"`
		IsDefault      bool   `json:"isDefault"`
	} `json:"audioTrack"`
}

type RapidAPIDetailsResponse struct {
	Title         string `json:"title"`
	ChannelTitle  string `json:"channelTitle"`
	LengthSeconds any    `json:"lengthSeconds"`
	Videos        struct {
		Items []RapidAPIVideoItem `json:"items"`
	} `json:"videos"`
	Audios struct {
		Items []RapidAPIAudioItem `json:"items"`
	} `json:"audios"`
	Audio struct {
		Items []RapidAPIAudioItem `json:"items"`
	} `json:"audio"`
}

func parseFPS(val any) int {
	if val == nil {
		return 30
	}
	switch v := val.(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		n, _ := strconv.Atoi(v)
		if n > 0 {
			return n
		}
		return 30
	default:
		return 30
	}
}

func getRapidAPIKeys() []string {
	defaultKey := "ec2be8b14cmsh4a5fe1b3d472a19p13be78jsn655c7e8e540d"
	envKeys := os.Getenv("RAPIDAPI_KEYS")
	if envKeys == "" {
		return []string{defaultKey}
	}
	parts := strings.Split(envKeys, ",")
	var keys []string
	for _, p := range parts {
		trimmed := strings.TrimSpace(p)
		if trimmed != "" {
			keys = append(keys, trimmed)
		}
	}
	if len(keys) == 0 {
		return []string{defaultKey}
	}
	return keys
}

func extractVideoID(rawURL string) string {
	re := regexp.MustCompile(`(?i)(?:v=|\/embed\/|\/shorts\/|youtu\.be\/|\/v\/|\/live\/)([a-zA-Z0-9_-]{11})`)
	matches := re.FindStringSubmatch(rawURL)
	if len(matches) > 1 {
		return matches[1]
	}
	return ""
}

func getAudioLang(item RapidAPIAudioItem) (code string, name string, isOrig bool) {
	name = item.DisplayName
	if name == "" {
		name = item.AudioTrack.DisplayName
	}
	if name == "" {
		name = item.Language
	}

	code = strings.ToLower(item.LanguageCode)
	if code == "" {
		code = strings.ToLower(item.Language)
	}

	if code == "" && item.AudioTrack.ID != "" {
		idLower := strings.ToLower(item.AudioTrack.ID)
		if strings.HasPrefix(idLower, "fa") || strings.Contains(idLower, "per") || strings.Contains(idLower, "fas") || strings.Contains(idLower, "farsi") {
			code = "fa"
		} else if strings.HasPrefix(idLower, "en") || strings.Contains(idLower, "eng") {
			code = "en"
		} else if strings.HasPrefix(idLower, "es") || strings.Contains(idLower, "spa") {
			code = "es"
		} else if strings.HasPrefix(idLower, "ar") || strings.Contains(idLower, "ara") {
			code = "ar"
		}
	}

	nameLower := strings.ToLower(name)
	if item.IsOriginal || item.AudioTrack.AudioIsDefault || strings.Contains(nameLower, "original") || strings.Contains(nameLower, "اصلی") {
		isOrig = true
	}

	if code == "" {
		if strings.Contains(nameLower, "persian") || strings.Contains(nameLower, "farsi") || strings.Contains(nameLower, "فارسی") || strings.Contains(nameLower, "fa") {
			code = "fa"
			if name == "" {
				name = "فارسی"
			}
		} else if strings.Contains(nameLower, "english") || strings.Contains(nameLower, "en") || strings.Contains(nameLower, "انگلیسی") {
			code = "en"
			if name == "" {
				name = "English"
			}
		} else if strings.Contains(nameLower, "spanish") || strings.Contains(nameLower, "es") || strings.Contains(nameLower, "اسپانیایی") {
			code = "es"
		} else if strings.Contains(nameLower, "arabic") || strings.Contains(nameLower, "ar") || strings.Contains(nameLower, "عربی") {
			code = "ar"
		}
	}

	if code == "" && item.URL != "" {
		urlLower := strings.ToLower(item.URL)
		if strings.Contains(urlLower, "lang=fa") || strings.Contains(urlLower, "lang=fas") || strings.Contains(urlLower, "lang=per") || strings.Contains(urlLower, "audio_track=fa") {
			code = "fa"
			name = "فارسی"
		} else if strings.Contains(urlLower, "lang=en") || strings.Contains(urlLower, "audio_track=en") {
			code = "en"
			name = "English"
		}
	}

	if name == "" {
		name = item.Quality
	}
	return code, name, isOrig
}

func selectBestAudioStream(audios []RapidAPIAudioItem, targetLang, token string) (string, string) {
	if len(audios) == 0 {
		return "", ""
	}

	targetLang = strings.ToLower(strings.TrimSpace(targetLang))

	Logger.Info("EXTRACTOR", token, "Evaluating %d available audio track(s) for requested language [%s]", len(audios), targetLang)
	for i, item := range audios {
		c, n, orig := getAudioLang(item)
		Logger.Info("EXTRACTOR", token, "  Option #%d: Code='%s', Name='%s', Orig=%v, Quality='%s', TrackID='%s'", i+1, c, n, orig, item.Quality, item.AudioTrack.ID)
	}

	// ۱. اگر کاربر صریحاً زبانی را انتخاب کرده بود (مثلاً fa یا en یا orig)
	if targetLang != "" && targetLang != "default" {
		for _, item := range audios {
			code, name, isOrig := getAudioLang(item)
			if targetLang == "orig" && isOrig {
				Logger.Info("EXTRACTOR", token, "Matched requested Original Audio Track: %s (%s)", name, item.Quality)
				return item.URL, name
			}
			if targetLang == "fa" && (code == "fa" || strings.Contains(strings.ToLower(name), "persian") || strings.Contains(strings.ToLower(name), "farsi") || strings.Contains(name, "فارسی")) {
				Logger.Info("EXTRACTOR", token, "Matched requested Persian Audio Track: %s (%s)", name, item.Quality)
				return item.URL, name
			}
			if targetLang == "en" && (code == "en" || strings.Contains(strings.ToLower(name), "english") || strings.Contains(name, "انگلیسی")) {
				Logger.Info("EXTRACTOR", token, "Matched requested English Audio Track: %s (%s)", name, item.Quality)
				return item.URL, name
			}
			if code == targetLang || strings.EqualFold(code, targetLang) || strings.Contains(strings.ToLower(name), targetLang) {
				Logger.Info("EXTRACTOR", token, "Matched requested Audio Track [%s]: %s (%s)", targetLang, name, item.Quality)
				return item.URL, name
			}
		}
		Logger.Warn("EXTRACTOR", token, "Requested audio track [%s] not explicitly found among tracks, falling back to smart prioritization", targetLang)
	}

	// ۲. استراتژی هوشمند (Smart Priority):
	// اولویت اول: زبان فارسی اگر موجود باشد
	for _, item := range audios {
		code, name, _ := getAudioLang(item)
		if code == "fa" || strings.Contains(strings.ToLower(name), "persian") || strings.Contains(strings.ToLower(name), "farsi") || strings.Contains(name, "فارسی") {
			Logger.Info("EXTRACTOR", token, "Smart Priority Selected Persian Audio: %s (%s)", name, item.Quality)
			return item.URL, name
		}
	}

	// اولویت دوم: زبان اصلی ویدیو (Original Track)
	for _, item := range audios {
		_, name, isOrig := getAudioLang(item)
		if isOrig || item.IsOriginal || item.AudioTrack.AudioIsDefault || strings.Contains(strings.ToLower(name), "original") || strings.Contains(name, "اصلی") {
			Logger.Info("EXTRACTOR", token, "Smart Priority Selected Original Audio: %s (%s)", name, item.Quality)
			return item.URL, name
		}
	}

	// اولویت سوم: ترک پیش‌فرض یا بالاترین کیفیت
	for _, item := range audios {
		if item.IsDefault || item.AudioTrack.AudioIsDefault {
			_, name, _ := getAudioLang(item)
			Logger.Info("EXTRACTOR", token, "Auto-Selected Default Audio Track: %s (%s)", name, item.Quality)
			return item.URL, name
		}
	}

	_, name, _ := getAudioLang(audios[0])
	Logger.Info("EXTRACTOR", token, "Auto-Selected Standard Audio Track: %s (%s)", name, audios[0].Quality)
	return audios[0].URL, name
}

func ExtractFromRapidAPI(rawURL, quality, audioLang, token string) (*ExtractedMedia, error) {
	videoID := extractVideoID(rawURL)
	if videoID == "" {
		Logger.Error("EXTRACTOR", token, "Invalid YouTube URL provided: %s", rawURL)
		return nil, fmt.Errorf("invalid youtube url: %s", rawURL)
	}

	Logger.Info("EXTRACTOR", token, "Starting extraction for VideoID: %s, Requested Quality: %s, AudioLang: %s", videoID, quality, audioLang)

	keys := getRapidAPIKeys()
	client := &http.Client{Timeout: 25 * time.Second}

	var lastErr error
	for idx, key := range keys {
		maskedKey := "..." + key[len(key)-6:]
		Logger.Debug("EXTRACTOR", token, "Trying RapidAPI key #%d (%s)", idx+1, maskedKey)

		reqURL := fmt.Sprintf("https://youtube-media-downloader.p.rapidapi.com/v2/video/details?videoId=%s", url.QueryEscape(videoID))
		req, err := http.NewRequest("GET", reqURL, nil)
		if err != nil {
			Logger.Warn("EXTRACTOR", token, "Failed to create HTTP request for key #%d: %v", idx+1, err)
			lastErr = err
			continue
		}
		req.Header.Set("x-rapidapi-host", "youtube-media-downloader.p.rapidapi.com")
		req.Header.Set("x-rapidapi-key", key)
		req.Header.Set("Accept", "application/json")

		startTime := time.Now()
		resp, err := client.Do(req)
		elapsed := time.Since(startTime)

		if err != nil {
			Logger.Warn("EXTRACTOR", token, "RapidAPI request error with key #%d after %v: %v", idx+1, elapsed, err)
			lastErr = err
			continue
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			Logger.Warn("EXTRACTOR", token, "RapidAPI returned HTTP %d with key #%d in %v", resp.StatusCode, idx+1, elapsed)
			lastErr = fmt.Errorf("rapidapi returned HTTP %d", resp.StatusCode)
			continue
		}

		var data RapidAPIDetailsResponse
		err = json.NewDecoder(resp.Body).Decode(&data)
		resp.Body.Close()
		if err != nil {
			Logger.Warn("EXTRACTOR", token, "JSON decode error from RapidAPI with key #%d: %v", idx+1, err)
			lastErr = err
			continue
		}

		title := data.Title
		if title == "" {
			title = "YouTube Video " + videoID
		}

		allAudios := data.Audios.Items
		if len(allAudios) == 0 {
			allAudios = data.Audio.Items
		}
		for i := range allAudios {
			if allAudios[i].URL == "" {
				if allAudios[i].Link != "" {
					allAudios[i].URL = allAudios[i].Link
				} else if allAudios[i].DownloadURL != "" {
					allAudios[i].URL = allAudios[i].DownloadURL
				}
			}
		}

		Logger.Info("EXTRACTOR", token, "Extracted Title: '%s', Channel: '%s', Duration: %v sec, Videos: %d, Audios: %d",
			title, data.ChannelTitle, data.LengthSeconds, len(data.Videos.Items), len(allAudios))

		// انتخاب هوشمند ترک صوتی بر اساس درخواست کاربر یا اولویت فارسی/اصلی
		bestAudioURL, selectedLangName := selectBestAudioStream(allAudios, audioLang, token)

		// اگر زبان فارسی یا ترک خاصی خواسته شده و از RapidAPI دریافت نشد، مستقیماً از Innertube یوتیوب استعلام می‌کنیم
		if (audioLang == "fa" && (bestAudioURL == "" || !strings.Contains(selectedLangName, "فارسی"))) || bestAudioURL == "" {
			inURL, inName, inErr := ExtractFromInnertube(videoID, audioLang, token)
			if inErr == nil && inURL != "" {
				bestAudioURL = inURL
				selectedLangName = inName
				Logger.Info("EXTRACTOR", token, "Resolved targeted audio stream via YouTube Innertube: %s", selectedLangName)
			}
		}

		isAudioOnly := (quality == "audio" || quality == "mp3" || quality == "m4a")
		if isAudioOnly {
			if bestAudioURL != "" {
				Logger.Info("EXTRACTOR", token, "Audio-only requested (%s). Successfully matched audio stream.", selectedLangName)
				return &ExtractedMedia{
					Type:          "audio",
					Title:         title,
					AudioURL:      bestAudioURL,
					AudioLangName: selectedLangName,
				}, nil
			}
			Logger.Warn("EXTRACTOR", token, "Audio requested but no audio stream found in RapidAPI response")
		}

		// Find matching video stream
		targetHeight := 1080
		want60fps := strings.Contains(quality, "60")
		if strings.Contains(quality, "2160") || strings.Contains(quality, "4k") {
			targetHeight = 2160
		} else if strings.Contains(quality, "1440") || strings.Contains(quality, "2k") {
			targetHeight = 1440
		} else if strings.Contains(quality, "1080") {
			targetHeight = 1080
		} else if strings.Contains(quality, "720") {
			targetHeight = 720
		} else if strings.Contains(quality, "480") {
			targetHeight = 480
		} else if strings.Contains(quality, "360") {
			targetHeight = 360
		} else if strings.Contains(quality, "240") {
			targetHeight = 240
		}

		var targetVideoURL string
		var hasAudio bool
		var selectedQuality string
		var selectedFPS any

		for _, v := range data.Videos.Items {
			if v.URL == "" {
				continue
			}
			qLower := strings.ToLower(v.Quality)
			if strings.Contains(qLower, strconv.Itoa(targetHeight)) {
				if want60fps && (parseFPS(v.FPS) < 50 && !strings.Contains(qLower, "60")) {
					if targetVideoURL == "" {
						targetVideoURL = v.URL
						hasAudio = v.HasAudio
						selectedQuality = v.Quality
						selectedFPS = v.FPS
					}
					continue
				}
				targetVideoURL = v.URL
				hasAudio = v.HasAudio
				selectedQuality = v.Quality
				selectedFPS = v.FPS
				break
			}
		}

		// Fallback to highest quality available if specific height not found
		if targetVideoURL == "" && len(data.Videos.Items) > 0 {
			targetVideoURL = data.Videos.Items[0].URL
			hasAudio = data.Videos.Items[0].HasAudio
			selectedQuality = data.Videos.Items[0].Quality
			selectedFPS = data.Videos.Items[0].FPS
			Logger.Warn("EXTRACTOR", token, "Exact match for %dp not found, falling back to: %s (fps: %v)", targetHeight, selectedQuality, selectedFPS)
		}

		if targetVideoURL != "" {
			audioURL := bestAudioURL
			if audioURL != "" {
				Logger.Info("EXTRACTOR", token, "Video stream (%s) attached with targeted audio track [%s] for FFmpeg audio replacement.", selectedQuality, selectedLangName)
			} else if hasAudio {
				Logger.Info("EXTRACTOR", token, "Video stream (%s) contains embedded audio.", selectedQuality)
			}
			return &ExtractedMedia{
				Type:          "video",
				Title:         title,
				VideoURL:      targetVideoURL,
				AudioURL:      audioURL,
				AudioLangName: selectedLangName,
				HasAudio:      hasAudio && audioURL == "",
			}, nil
		}
	}

	if lastErr != nil {
		Logger.Error("EXTRACTOR", token, "All RapidAPI keys exhausted. Last error: %v", lastErr)
		return nil, lastErr
	}
	Logger.Error("EXTRACTOR", token, "No suitable video stream found in RapidAPI response")
	return nil, fmt.Errorf("no suitable video stream found")
}

func ExtractFromInnertube(videoID, targetLang, token string) (string, string, error) {
	postBody, _ := json.Marshal(map[string]any{
		"context": map[string]any{
			"client": map[string]any{
				"clientName":    "ANDROID",
				"clientVersion": "19.09.37",
				"hl":            "fa",
				"gl":            "IR",
			},
		},
		"videoId": videoID,
	})

	req, err := http.NewRequest("POST", "https://www.youtube.com/youtubei/v1/player?prettyPrint=false", bytes.NewReader(postBody))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "com.google.android.youtube/19.09.37 (Linux; U; Android 11) gzip")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	var pData struct {
		StreamingData struct {
			AdaptiveFormats []struct {
				Itag             int    `json:"itag"`
				URL              string `json:"url"`
				MimeType         string `json:"mimeType"`
				Bitrate          int    `json:"bitrate"`
				AudioQuality     string `json:"audioQuality"`
				ApproxDurationMs string `json:"approxDurationMs"`
				AudioTrack       struct {
					ID             string `json:"id"`
					DisplayName    string `json:"displayName"`
					AudioIsDefault bool   `json:"audioIsDefault"`
				} `json:"audioTrack"`
			} `json:"adaptiveFormats"`
		} `json:"streamingData"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&pData); err != nil {
		return "", "", err
	}

	targetLang = strings.ToLower(strings.TrimSpace(targetLang))
	Logger.Info("INNERTUBE", token, "YouTube Innertube returned %d adaptive format(s)", len(pData.StreamingData.AdaptiveFormats))

	var bestAudioURL, matchedName string
	var bestBitrate int

	for _, f := range pData.StreamingData.AdaptiveFormats {
		if !strings.HasPrefix(f.MimeType, "audio/") || f.URL == "" {
			continue
		}

		tId := strings.ToLower(f.AudioTrack.ID)
		dName := f.AudioTrack.DisplayName
		dNameLower := strings.ToLower(dName)
		isFa := strings.HasPrefix(tId, "fa") || strings.Contains(tId, "per") || strings.Contains(dNameLower, "persian") || strings.Contains(dNameLower, "farsi") || strings.Contains(dNameLower, "فارسی")
		isEn := strings.HasPrefix(tId, "en") || strings.Contains(dNameLower, "english")
		isOrig := f.AudioTrack.AudioIsDefault || strings.Contains(dNameLower, "original") || strings.Contains(dNameLower, "اصلی")

		if targetLang == "fa" && isFa {
			if f.Bitrate > bestBitrate {
				bestBitrate = f.Bitrate
				bestAudioURL = f.URL
				matchedName = "فارسی (" + dName + ")"
			}
		} else if targetLang == "en" && isEn {
			if f.Bitrate > bestBitrate {
				bestBitrate = f.Bitrate
				bestAudioURL = f.URL
				matchedName = "English (" + dName + ")"
			}
		} else if (targetLang == "orig" || targetLang == "default") && isOrig {
			if f.Bitrate > bestBitrate {
				bestBitrate = f.Bitrate
				bestAudioURL = f.URL
				matchedName = "Original (" + dName + ")"
			}
		}
	}

	if bestAudioURL != "" {
		Logger.Info("INNERTUBE", token, "Successfully resolved audio from YouTube Innertube: %s", matchedName)
		return bestAudioURL, matchedName, nil
	}

	return "", "", fmt.Errorf("no matching audio track in Innertube for lang: %s", targetLang)
}
