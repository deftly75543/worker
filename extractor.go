package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
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
	Quality     string `json:"quality"`
	FPS         any    `json:"fps"`
	URL         string `json:"url"`
	Link        string `json:"link"`
	DownloadURL string `json:"downloadUrl"`
	HasAudio    bool   `json:"hasAudio"`
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

func getRapidAPIKeys(customKeys string) []string {
	defaultKey := "6a44c52b98mshf8d49aef80a8607p1ad3d4jsn3bbeb1e34dc8"
	rawKeys := strings.TrimSpace(customKeys)
	if rawKeys == "" {
		rawKeys = strings.TrimSpace(os.Getenv("RAPIDAPI_KEYS"))
	}
	if rawKeys == "" {
		return []string{defaultKey}
	}
	parts := strings.Split(rawKeys, ",")
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

	// ۲. استخراج صوت طبیعی و اصلی خود ویدیو (Original Native Audio):
	// اولویت اول: زبان و صوت اصلی خود ویدیو (Original Track)
	for _, item := range audios {
		_, name, isOrig := getAudioLang(item)
		if isOrig || item.IsOriginal || item.AudioTrack.AudioIsDefault || strings.Contains(strings.ToLower(name), "original") || strings.Contains(name, "اصلی") {
			Logger.Info("EXTRACTOR", token, "Selected Original Native Audio Track: %s (%s)", name, item.Quality)
			return item.URL, name
		}
	}

	// اولویت دوم: ترک پیش‌فرض یا بالاترین کیفیت
	for _, item := range audios {
		if item.IsDefault || item.AudioTrack.AudioIsDefault {
			_, name, _ := getAudioLang(item)
			Logger.Info("EXTRACTOR", token, "Selected Default Audio Track: %s (%s)", name, item.Quality)
			return item.URL, name
		}
	}

	_, name, _ := getAudioLang(audios[0])
	Logger.Info("EXTRACTOR", token, "Selected Standard Audio Track: %s (%s)", name, audios[0].Quality)
	return audios[0].URL, name
}

func ExtractFromRapidAPI(rawURL, quality, audioLang, customKeys, token string) (*ExtractedMedia, error) {
	videoID := extractVideoID(rawURL)
	if videoID == "" {
		Logger.Error("EXTRACTOR", token, "Invalid YouTube URL provided: %s", rawURL)
		return nil, fmt.Errorf("invalid youtube url: %s", rawURL)
	}

	Logger.Info("EXTRACTOR", token, "Starting extraction for VideoID: %s, Requested Quality: %s, AudioLang: %s", videoID, quality, audioLang)

	keys := getRapidAPIKeys(customKeys)
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
			bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			resp.Body.Close()
			Logger.Warn("EXTRACTOR", token, "RapidAPI #1 returned HTTP %d with key #%d in %v: %s", resp.StatusCode, idx+1, elapsed, string(bodyBytes))
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

		// اگر ترکی از وب‌سرویس دریافت نشد، به عنوان آخرین راهکار از اینرتیوب استعلام می‌کنیم
		if bestAudioURL == "" {
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

		for i := range data.Videos.Items {
			if data.Videos.Items[i].URL == "" {
				if data.Videos.Items[i].Link != "" {
					data.Videos.Items[i].URL = data.Videos.Items[i].Link
				} else if data.Videos.Items[i].DownloadURL != "" {
					data.Videos.Items[i].URL = data.Videos.Items[i].DownloadURL
				}
			}
		}

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

	Logger.Warn("EXTRACTOR", token, "Primary RapidAPI provider failed or rate-limited. Falling back to Cloud Api Hub provider...")
	cloudMedia, cloudErr := extractFromCloudApiHub(videoID, quality, audioLang, keys, token)
	if cloudErr == nil && cloudMedia != nil {
		return cloudMedia, nil
	}

	Logger.Warn("EXTRACTOR", token, "Cloud Api Hub provider failed (%v). Falling back to YouTube Info & Download API provider...", cloudErr)
	infoMedia, infoErr := extractFromYouTubeInfoDownloadAPI(videoID, quality, audioLang, keys, token)
	if infoErr == nil && infoMedia != nil {
		return infoMedia, nil
	}

	Logger.Warn("EXTRACTOR", token, "YouTube Info & Download API failed (%v). Falling back to YouTube Video And Shorts Downloader provider...", infoErr)
	shortsMedia, shortsErr := extractFromYouTubeVideoAndShortsDownloader(videoID, quality, audioLang, keys, token)
	if shortsErr == nil && shortsMedia != nil {
		return shortsMedia, nil
	}

	Logger.Warn("EXTRACTOR", token, "YouTube Video And Shorts Downloader failed (%v). Falling back to YouTube Video And Shorts Downloader V2 provider...", shortsErr)
	v2Media, v2Err := extractFromYouTubeVideoAndShortsDownloaderV2(videoID, quality, audioLang, keys, token)
	if v2Err == nil && v2Media != nil {
		return v2Media, nil
	}

	Logger.Warn("EXTRACTOR", token, "YouTube Video And Shorts Downloader V2 failed (%v). Falling back to YouTube MP4/MP3 Downloader provider...", v2Err)
	mp4Media, mp4Err := extractFromYouTubeMp4Mp3Downloader(videoID, quality, audioLang, keys, token)
	if mp4Err == nil && mp4Media != nil {
		return mp4Media, nil
	}

	Logger.Warn("EXTRACTOR", token, "YouTube MP4/MP3 Downloader failed (%v). Falling back to Ziyotech Youtube Downloader API provider...", mp4Err)
	ziyoMedia, ziyoErr := extractFromZiyotech(videoID, quality, audioLang, keys, token)
	if ziyoErr == nil && ziyoMedia != nil {
		return ziyoMedia, nil
	}

	Logger.Warn("EXTRACTOR", token, "Ziyotech API failed (%v). Falling back to YouTube Quick Video Downloader provider...", ziyoErr)
	quickMedia, quickErr := extractFromYouTubeQuickVideoDownloader(videoID, quality, audioLang, keys, token)
	if quickErr == nil && quickMedia != nil {
		return quickMedia, nil
	}

	Logger.Warn("EXTRACTOR", token, "YouTube Quick Video Downloader failed (%v). Falling back to YouTube138 API provider...", quickErr)
	yt138Media, yt138Err := extractFromYouTube138(videoID, quality, audioLang, keys, token)
	if yt138Err == nil && yt138Media != nil {
		return yt138Media, nil
	}

	if yt138Err != nil {
		lastErr = yt138Err
	}

	if lastErr != nil {
		Logger.Error("EXTRACTOR", token, "All RapidAPI providers and fallbacks exhausted. Last error: %v", lastErr)
		return nil, lastErr
	}
	Logger.Error("EXTRACTOR", token, "No suitable video stream found in any provider")
	return nil, fmt.Errorf("no suitable video stream found")
}

func ExtractDirectFromInnertube(videoID, quality, audioLang, token string) (*ExtractedMedia, error) {
	postBody, _ := json.Marshal(map[string]any{
		"context": map[string]any{
			"client": map[string]any{
				"clientName":    "ANDROID",
				"clientVersion": "19.09.37",
				"hl":            "fa",
				"gl":            "US",
			},
		},
		"videoId": videoID,
	})

	req, err := http.NewRequest("POST", "https://www.youtube.com/youtubei/v1/player?prettyPrint=false", bytes.NewReader(postBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "com.google.android.youtube/19.09.37 (Linux; U; Android 11) gzip")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var pData struct {
		VideoDetails struct {
			Title  string `json:"title"`
			Author string `json:"author"`
		} `json:"videoDetails"`
		StreamingData struct {
			Formats []struct {
				URL          string `json:"url"`
				QualityLabel string `json:"qualityLabel"`
				Quality      string `json:"quality"`
			} `json:"formats"`
			AdaptiveFormats []struct {
				URL          string `json:"url"`
				MimeType     string `json:"mimeType"`
				Bitrate      int    `json:"bitrate"`
				QualityLabel string `json:"qualityLabel"`
				AudioQuality string `json:"audioQuality"`
			} `json:"adaptiveFormats"`
		} `json:"streamingData"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&pData); err != nil {
		return nil, err
	}

	title := pData.VideoDetails.Title
	if title == "" {
		title = "YouTube Video " + videoID
	}

	var bestVideoURL, bestAudioURL string
	var bestAudioBitrate int
	var hasAudio bool

	for _, f := range pData.StreamingData.Formats {
		if f.URL == "" {
			continue
		}
		if strings.Contains(strings.ToLower(f.QualityLabel), quality) || strings.Contains(strings.ToLower(f.Quality), quality) {
			bestVideoURL = f.URL
			hasAudio = true
			break
		}
	}

	for _, f := range pData.StreamingData.AdaptiveFormats {
		if f.URL == "" {
			continue
		}
		if strings.HasPrefix(f.MimeType, "video/") {
			if strings.Contains(strings.ToLower(f.QualityLabel), quality) {
				bestVideoURL = f.URL
			} else if bestVideoURL == "" && strings.Contains(strings.ToLower(f.QualityLabel), "720") {
				bestVideoURL = f.URL
			} else if bestVideoURL == "" {
				bestVideoURL = f.URL
			}
		} else if strings.HasPrefix(f.MimeType, "audio/") {
			if f.Bitrate > bestAudioBitrate {
				bestAudioBitrate = f.Bitrate
				bestAudioURL = f.URL
			}
		}
	}

	isAudioOnly := (quality == "audio" || quality == "mp3" || quality == "m4a")
	if isAudioOnly {
		if bestAudioURL == "" {
			bestAudioURL = bestVideoURL
		}
		if bestAudioURL != "" {
			Logger.Info("INNERTUBE", token, "Extracted direct audio from YouTube Innertube")
			return &ExtractedMedia{
				Type:          "audio",
				Title:         title,
				AudioURL:      bestAudioURL,
				AudioLangName: "صوت اصلی",
				HasAudio:      true,
			}, nil
		}
	}

	if bestVideoURL != "" {
		Logger.Info("INNERTUBE", token, "Extracted direct video from YouTube Innertube (100%% Fail-Safe)")
		return &ExtractedMedia{
			Type:          "video",
			Title:         title,
			VideoURL:      bestVideoURL,
			AudioURL:      bestAudioURL,
			AudioLangName: "پیش‌فرض",
			HasAudio:      hasAudio || (bestAudioURL == ""),
		}, nil
	}

	return nil, fmt.Errorf("no direct streams in innertube")
}

func ExtractFromYtDlp(videoID, quality, audioLang, token string) (*ExtractedMedia, error) {
	vURL := fmt.Sprintf("https://www.youtube.com/watch?v=%s", videoID)

	cmdCheck := exec.Command("yt-dlp", "--version")
	if err := cmdCheck.Run(); err != nil {
		return nil, fmt.Errorf("yt-dlp binary not found: %w", err)
	}

	formatSelector := fmt.Sprintf("best[height<=%s]/bestvideo[height<=%s]+bestaudio/best", quality, quality)
	if quality == "audio" || quality == "mp3" || quality == "m4a" {
		formatSelector = "bestaudio/best"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "yt-dlp",
		"--no-warnings",
		"--no-check-certificates",
		"--extractor-args", "youtube:player_client=android,ios,web",
		"-g",
		"-f", formatSelector,
		vURL,
	)
	out, err := cmd.Output()
	if err != nil {
		Logger.Warn("EXTRACTOR", token, "yt-dlp android client failed, trying generic fallback: %v", err)
		cmdFallback := exec.CommandContext(ctx, "yt-dlp", "--no-warnings", "-g", "-f", formatSelector, vURL)
		out, err = cmdFallback.Output()
		if err != nil {
			return nil, fmt.Errorf("yt-dlp extraction error: %w", err)
		}
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 0 || lines[0] == "" {
		return nil, fmt.Errorf("yt-dlp returned no URLs")
	}

	title := "YouTube Video " + videoID
	titleCmd := exec.CommandContext(ctx, "yt-dlp", "--get-title", "--no-warnings", vURL)
	if titleBytes, tErr := titleCmd.Output(); tErr == nil {
		if tStr := strings.TrimSpace(string(titleBytes)); tStr != "" {
			title = tStr
		}
	}

	if len(lines) == 1 {
		isAudio := (quality == "audio" || quality == "mp3" || quality == "m4a")
		if isAudio {
			return &ExtractedMedia{
				Type:          "audio",
				Title:         title,
				AudioURL:      lines[0],
				AudioLangName: "صوت اصلی",
				HasAudio:      true,
			}, nil
		}
		return &ExtractedMedia{
			Type:          "video",
			Title:         title,
			VideoURL:      lines[0],
			AudioURL:      "",
			AudioLangName: "پیش‌فرض",
			HasAudio:      true,
		}, nil
	}

	return &ExtractedMedia{
		Type:          "video",
		Title:         title,
		VideoURL:      lines[0],
		AudioURL:      lines[1],
		AudioLangName: "پیش‌فرض",
		HasAudio:      false,
	}, nil
}

func extractFromCloudApiHub(videoID, quality, audioLang string, keys []string, token string) (*ExtractedMedia, error) {
	client := &http.Client{Timeout: 25 * time.Second}
	var lastErr error

	for idx, key := range keys {
		maskedKey := "..." + key[len(key)-6:]
		Logger.Info("EXTRACTOR", token, "Trying Cloud Api Hub provider with key #%d (%s)", idx+1, maskedKey)

		reqURL := fmt.Sprintf("https://cloud-api-hub-youtube-downloader.p.rapidapi.com/download?id=%s", url.QueryEscape(videoID))
		req, err := http.NewRequest("GET", reqURL, nil)
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("x-rapidapi-host", "cloud-api-hub-youtube-downloader.p.rapidapi.com")
		req.Header.Set("x-rapidapi-key", key)
		req.Header.Set("Accept", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode != http.StatusOK {
			bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			resp.Body.Close()
			Logger.Warn("EXTRACTOR", token, "Cloud Api Hub returned HTTP %d: %s", resp.StatusCode, string(bodyBytes))
			lastErr = fmt.Errorf("cloud api hub returned HTTP %d", resp.StatusCode)
			continue
		}

		var rawVal any
		err = json.NewDecoder(resp.Body).Decode(&rawVal)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}

		title := fmt.Sprintf("YouTube Video %s", videoID)
		var formatItems []any

		if arr, ok := rawVal.([]any); ok {
			formatItems = arr
		} else if rawMap, ok := rawVal.(map[string]any); ok {
			if t, ok := rawMap["title"].(string); ok && t != "" {
				title = t
			}
			if f, ok := rawMap["formats"].([]any); ok {
				formatItems = f
			} else if dMap, ok := rawMap["data"].(map[string]any); ok {
				if t, ok := dMap["title"].(string); ok && t != "" {
					title = t
				}
				if df, ok := dMap["formats"].([]any); ok {
					formatItems = df
				} else if dv, ok := dMap["videos"].([]any); ok {
					formatItems = dv
				}
			} else if d, ok := rawMap["data"].([]any); ok {
				formatItems = d
			} else if v, ok := rawMap["videos"].([]any); ok {
				formatItems = v
			}
		}

		var videoURL, audioURL string
		hasAudio := false

		for _, item := range formatItems {
			im, ok := item.(map[string]any)
			if !ok {
				continue
			}

			u := ""
			if uStr, ok := im["url"].(string); ok {
				u = uStr
			} else if lStr, ok := im["link"].(string); ok {
				u = lStr
			} else if dStr, ok := im["downloadUrl"].(string); ok {
				u = dStr
			}

			if u == "" {
				continue
			}

			qStr := strings.ToLower(fmt.Sprintf("%v", im["qualityLabel"]))
			if qStr == "" || qStr == "<nil>" {
				qStr = strings.ToLower(fmt.Sprintf("%v", im["quality"]))
			}

			mimeStr := strings.ToLower(fmt.Sprintf("%v", im["mimeType"]))
			if mimeStr == "" || mimeStr == "<nil>" {
				mimeStr = strings.ToLower(fmt.Sprintf("%v", im["type"]))
			}

			// فیلتر کامل و قطعی تصاویر بندانگشتی و استوری‌بردها (webp / jpeg / sprite sheets)
			if strings.Contains(mimeStr, "image") || strings.Contains(mimeStr, "webp") || strings.Contains(u, "storyboard") || strings.Contains(u, "mime=image") || strings.Contains(qStr, "storyboard") {
				continue
			}

			isAudioStream := strings.Contains(mimeStr, "audio") || strings.Contains(u, "mime=audio")
			itemHasAudio := false
			if ha, ok := im["hasAudio"].(bool); ok {
				itemHasAudio = ha
			}
			itemHasVideo := true
			if hv, ok := im["hasVideo"].(bool); ok {
				itemHasVideo = hv
			}

			targetHeight := 720
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

			if isAudioStream || (!itemHasVideo && itemHasAudio) {
				if audioURL == "" {
					audioURL = u
				}
			} else if itemHasVideo {
				if strings.Contains(qStr, strconv.Itoa(targetHeight)) || strings.Contains(qStr, quality) {
					videoURL = u
					hasAudio = itemHasAudio
				}
			}
		}

		if videoURL == "" && !isAudioOnly {
			// اگر کیفیت درخواستی در این پروایدر پیدا نشد، به پروایدر بعدی سوییچ کن
			lastErr = fmt.Errorf("cloud api hub does not have requested quality %s", quality)
			continue
		}

		isAudioOnly := (quality == "audio" || quality == "mp3" || quality == "m4a")
		if isAudioOnly {
			if audioURL == "" {
				audioURL = videoURL
			}
			if audioURL != "" {
				Logger.Info("EXTRACTOR", token, "Successfully extracted audio from Cloud Api Hub")
				return &ExtractedMedia{
					Type:          "audio",
					Title:         title,
					AudioURL:      audioURL,
					AudioLangName: "صوت اصلی",
					HasAudio:      true,
				}, nil
			}
		}

		if videoURL != "" {
			Logger.Info("EXTRACTOR", token, "Successfully extracted video from Cloud Api Hub")
			if audioURL == "" && !hasAudio {
				inURL, _, inErr := ExtractFromInnertube(videoID, audioLang, token)
				if inErr == nil && inURL != "" {
					audioURL = inURL
				}
			}
			return &ExtractedMedia{
				Type:          "video",
				Title:         title,
				VideoURL:      videoURL,
				AudioURL:      audioURL,
				AudioLangName: "پیش‌فرض",
				HasAudio:      hasAudio && audioURL == "",
			}, nil
		}
	}

	return nil, lastErr
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

func extractFromYouTubeInfoDownloadAPI(videoID, quality, audioLang string, keys []string, token string) (*ExtractedMedia, error) {
	client := &http.Client{Timeout: 25 * time.Second}
	var lastErr error

	format := quality
	if quality == "audio" || quality == "mp3" || quality == "m4a" {
		format = "mp3"
	} else if quality == "1080p60" || quality == "1080" {
		format = "1080"
	} else if quality == "720p60" || quality == "720" {
		format = "720"
	} else if quality == "480" {
		format = "480"
	} else if quality == "360" {
		format = "360"
	} else {
		format = "720"
	}

	for idx, key := range keys {
		maskedKey := "..." + key[len(key)-6:]
		Logger.Info("EXTRACTOR", token, "Trying YouTube Info & Download API with key #%d (%s)", idx+1, maskedKey)

		vURL := fmt.Sprintf("https://www.youtube.com/watch?v=%s", videoID)
		reqURL := fmt.Sprintf("https://youtube-info-download-api.p.rapidapi.com/ajax/download.php?format=%s&add_info=0&url=%s&audio_quality=128&allow_extended_duration=false&no_merge=false&audio_language=en",
			url.QueryEscape(format), url.QueryEscape(vURL))

		req, err := http.NewRequest("GET", reqURL, nil)
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("x-rapidapi-host", "youtube-info-download-api.p.rapidapi.com")
		req.Header.Set("x-rapidapi-key", key)
		req.Header.Set("Accept", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode != http.StatusOK {
			bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			resp.Body.Close()
			lastErr = fmt.Errorf("youtube-info-download-api returned HTTP %d: %s", resp.StatusCode, string(bodyBytes))
			continue
		}

		var rawVal any
		err = json.NewDecoder(resp.Body).Decode(&rawVal)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}

		title := fmt.Sprintf("YouTube Video %s", videoID)
		streamURL := ""

		if rawMap, ok := rawVal.(map[string]any); ok {
			for _, k := range []string{"url", "link", "download_url", "downloadUrl", "download", "file", "media_url", "direct_link"} {
				if s, ok := rawMap[k].(string); ok && s != "" {
					streamURL = s
					break
				}
			}
			if t, ok := rawMap["title"].(string); ok && t != "" {
				title = t
			}
			if streamURL == "" {
				if dMap, ok := rawMap["data"].(map[string]any); ok {
					for _, k := range []string{"url", "link", "download_url", "downloadUrl", "download", "file"} {
						if s, ok := dMap[k].(string); ok && s != "" {
							streamURL = s
							break
						}
					}
					if t, ok := dMap["title"].(string); ok && t != "" {
						title = t
					}
				}
				if rMap, ok := rawMap["result"].(map[string]any); ok {
					for _, k := range []string{"url", "link", "download_url", "downloadUrl", "download"} {
						if s, ok := rMap[k].(string); ok && s != "" {
							streamURL = s
							break
						}
					}
				}
			}
		}

		if streamURL != "" {
			Logger.Info("EXTRACTOR", token, "Successfully extracted stream from YouTube Info & Download API!")
			isAudioOnly := (quality == "audio" || quality == "mp3" || quality == "m4a")
			if isAudioOnly {
				return &ExtractedMedia{
					Type:          "audio",
					Title:         title,
					AudioURL:      streamURL,
					AudioLangName: "صوت اصلی",
					HasAudio:      true,
				}, nil
			}

			audioURL := ""
			inURL, _, inErr := ExtractFromInnertube(videoID, audioLang, token)
			if inErr == nil && inURL != "" {
				audioURL = inURL
			}

			return &ExtractedMedia{
				Type:          "video",
				Title:         title,
				VideoURL:      streamURL,
				AudioURL:      audioURL,
				AudioLangName: "پیش‌فرض",
				HasAudio:      audioURL == "",
			}, nil
		}
	}

	return nil, lastErr
}

func extractFromYouTubeVideoAndShortsDownloader(videoID, quality, audioLang string, keys []string, token string) (*ExtractedMedia, error) {
	client := &http.Client{Timeout: 25 * time.Second}
	var lastErr error

	for idx, key := range keys {
		maskedKey := "..." + key[len(key)-6:]
		Logger.Info("EXTRACTOR", token, "Trying YouTube Video And Shorts Downloader with key #%d (%s)", idx+1, maskedKey)

		reqURL := fmt.Sprintf("https://youtube-video-and-shorts-downloader1.p.rapidapi.com/youtube/v3/video/details?videoId=%s", url.QueryEscape(videoID))
		req, err := http.NewRequest("GET", reqURL, nil)
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("x-rapidapi-host", "youtube-video-and-shorts-downloader1.p.rapidapi.com")
		req.Header.Set("x-rapidapi-key", key)
		req.Header.Set("Accept", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			lastErr = fmt.Errorf("youtube-video-and-shorts-downloader1 returned HTTP %d", resp.StatusCode)
			continue
		}

		var rawMap map[string]any
		err = json.NewDecoder(resp.Body).Decode(&rawMap)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}

		title := fmt.Sprintf("YouTube Video %s", videoID)
		if t, ok := rawMap["title"].(string); ok && t != "" {
			title = t
		}

		var formatItems []any
		if v, ok := rawMap["videos"].([]any); ok {
			formatItems = v
		} else if f, ok := rawMap["formats"].([]any); ok {
			formatItems = f
		} else if dMap, ok := rawMap["data"].(map[string]any); ok {
			if t, ok := dMap["title"].(string); ok && t != "" {
				title = t
			}
			if dv, ok := dMap["videos"].([]any); ok {
				formatItems = dv
			} else if df, ok := dMap["formats"].([]any); ok {
				formatItems = df
			}
		} else if d, ok := rawMap["data"].([]any); ok {
			formatItems = d
		}

		var videoURL, audioURL string
		hasAudio := false

		for _, item := range formatItems {
			im, ok := item.(map[string]any)
			if !ok {
				continue
			}

			u := ""
			if uStr, ok := im["url"].(string); ok {
				u = uStr
			} else if lStr, ok := im["link"].(string); ok {
				u = lStr
			} else if dStr, ok := im["downloadUrl"].(string); ok {
				u = dStr
			}

			if u == "" {
				continue
			}

			qStr := strings.ToLower(fmt.Sprintf("%v", im["quality"]))
			if qStr == "" || qStr == "<nil>" {
				qStr = strings.ToLower(fmt.Sprintf("%v", im["qualityLabel"]))
			}

			// فیلتر تصاویر و استوری‌برد
			if strings.Contains(mimeStr, "image") || strings.Contains(mimeStr, "webp") || strings.Contains(u, "storyboard") || strings.Contains(u, "mime=image") || strings.Contains(qStr, "storyboard") {
				continue
			}

			targetHeight := 720
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

			isAudioStream := strings.Contains(mimeStr, "audio") || strings.Contains(u, "mime=audio")
			itemHasAudio := false
			if ha, ok := im["hasAudio"].(bool); ok {
				itemHasAudio = ha
			}
			itemHasVideo := true
			if hv, ok := im["hasVideo"].(bool); ok {
				itemHasVideo = hv
			}

			if isAudioStream || (!itemHasVideo && itemHasAudio) {
				if audioURL == "" {
					audioURL = u
				}
			} else if itemHasVideo {
				if strings.Contains(qStr, strconv.Itoa(targetHeight)) || strings.Contains(qStr, quality) {
					videoURL = u
					hasAudio = itemHasAudio
				}
			}
		}

		if videoURL == "" && !isAudioOnly {
			lastErr = fmt.Errorf("video-and-shorts downloader does not have requested quality %s", quality)
			continue
		}

		isAudioOnly := (quality == "audio" || quality == "mp3" || quality == "m4a")
		if isAudioOnly {
			if audioURL == "" {
				audioURL = videoURL
			}
			if audioURL != "" {
				Logger.Info("EXTRACTOR", token, "Successfully extracted audio from YouTube Video And Shorts Downloader")
				return &ExtractedMedia{
					Type:          "audio",
					Title:         title,
					AudioURL:      audioURL,
					AudioLangName: "صوت اصلی",
					HasAudio:      true,
				}, nil
			}
		}

		if videoURL != "" {
			Logger.Info("EXTRACTOR", token, "Successfully extracted video from YouTube Video And Shorts Downloader")
			if audioURL == "" && !hasAudio {
				inURL, _, inErr := ExtractFromInnertube(videoID, audioLang, token)
				if inErr == nil && inURL != "" {
					audioURL = inURL
				}
			}
			return &ExtractedMedia{
				Type:          "video",
				Title:         title,
				VideoURL:      videoURL,
				AudioURL:      audioURL,
				AudioLangName: "پیش‌فرض",
				HasAudio:      hasAudio && audioURL == "",
			}, nil
		}
	}

	return nil, lastErr
}

func extractFromYouTubeVideoAndShortsDownloaderV2(videoID, quality, audioLang string, keys []string, token string) (*ExtractedMedia, error) {
	client := &http.Client{Timeout: 25 * time.Second}
	var lastErr error

	for idx, key := range keys {
		maskedKey := "..." + key[len(key)-6:]
		Logger.Info("EXTRACTOR", token, "Trying YouTube Video And Shorts Downloader V2 with key #%d (%s)", idx+1, maskedKey)

		reqURL := fmt.Sprintf("https://youtube-video-and-shorts-downloader.p.rapidapi.com/download.php?id=%s", url.QueryEscape(videoID))
		req, err := http.NewRequest("GET", reqURL, nil)
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("x-rapidapi-host", "youtube-video-and-shorts-downloader.p.rapidapi.com")
		req.Header.Set("x-rapidapi-key", key)
		req.Header.Set("Accept", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			lastErr = fmt.Errorf("youtube-video-and-shorts-downloader (V2) returned HTTP %d", resp.StatusCode)
			continue
		}

		var rawMap map[string]any
		err = json.NewDecoder(resp.Body).Decode(&rawMap)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}

		title := fmt.Sprintf("YouTube Video %s", videoID)
		if t, ok := rawMap["title"].(string); ok && t != "" {
			title = t
		}

		var formatItems []any
		if v, ok := rawMap["videos"].([]any); ok {
			formatItems = v
		} else if f, ok := rawMap["formats"].([]any); ok {
			formatItems = f
		} else if dMap, ok := rawMap["data"].(map[string]any); ok {
			if t, ok := dMap["title"].(string); ok && t != "" {
				title = t
			}
			if dv, ok := dMap["videos"].([]any); ok {
				formatItems = dv
			} else if df, ok := dMap["formats"].([]any); ok {
				formatItems = df
			}
		} else if d, ok := rawMap["data"].([]any); ok {
			formatItems = d
		}

		var videoURL, audioURL string
		hasAudio := false

		for _, item := range formatItems {
			im, ok := item.(map[string]any)
			if !ok {
				continue
			}

			u := ""
			if uStr, ok := im["url"].(string); ok {
				u = uStr
			} else if lStr, ok := im["link"].(string); ok {
				u = lStr
			} else if dStr, ok := im["downloadUrl"].(string); ok {
				u = dStr
			}

			if u == "" {
				continue
			}

			mimeStr := strings.ToLower(fmt.Sprintf("%v", im["mimeType"]))
			if mimeStr == "" || mimeStr == "<nil>" {
				mimeStr = strings.ToLower(fmt.Sprintf("%v", im["type"]))
			}

			// فیلتر تصاویر و استوری‌برد
			if strings.Contains(mimeStr, "image") || strings.Contains(mimeStr, "webp") || strings.Contains(u, "storyboard") || strings.Contains(u, "mime=image") || strings.Contains(qStr, "storyboard") {
				continue
			}

			targetHeight := 720
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

			isAudioStream := strings.Contains(mimeStr, "audio") || strings.Contains(u, "mime=audio")
			itemHasAudio := false
			if ha, ok := im["hasAudio"].(bool); ok {
				itemHasAudio = ha
			}
			itemHasVideo := true
			if hv, ok := im["hasVideo"].(bool); ok {
				itemHasVideo = hv
			}

			if isAudioStream || (!itemHasVideo && itemHasAudio) {
				if audioURL == "" {
					audioURL = u
				}
			} else if itemHasVideo {
				if strings.Contains(qStr, strconv.Itoa(targetHeight)) || strings.Contains(qStr, quality) {
					videoURL = u
					hasAudio = itemHasAudio
				}
			}
		}

		if videoURL == "" && !isAudioOnly {
			lastErr = fmt.Errorf("video-and-shorts downloader (V2) does not have requested quality %s", quality)
			continue
		}

		isAudioOnly := (quality == "audio" || quality == "mp3" || quality == "m4a")
		if isAudioOnly {
			if audioURL == "" {
				audioURL = videoURL
			}
			if audioURL != "" {
				Logger.Info("EXTRACTOR", token, "Successfully extracted audio from YouTube Video And Shorts Downloader V2")
				return &ExtractedMedia{
					Type:          "audio",
					Title:         title,
					AudioURL:      audioURL,
					AudioLangName: "صوت اصلی",
					HasAudio:      true,
				}, nil
			}
		}

		if videoURL != "" {
			Logger.Info("EXTRACTOR", token, "Successfully extracted video from YouTube Video And Shorts Downloader V2")
			if audioURL == "" && !hasAudio {
				inURL, _, inErr := ExtractFromInnertube(videoID, audioLang, token)
				if inErr == nil && inURL != "" {
					audioURL = inURL
				}
			}
			return &ExtractedMedia{
				Type:          "video",
				Title:         title,
				VideoURL:      videoURL,
				AudioURL:      audioURL,
				AudioLangName: "پیش‌فرض",
				HasAudio:      hasAudio && audioURL == "",
			}, nil
		}
	}

	return nil, lastErr
}

func extractFromYouTubeMp4Mp3Downloader(videoID, quality, audioLang string, keys []string, token string) (*ExtractedMedia, error) {
	client := &http.Client{Timeout: 25 * time.Second}
	var lastErr error

	format := quality
	if quality == "audio" || quality == "mp3" || quality == "m4a" {
		format = "mp3"
	} else if quality == "1080p60" || quality == "1080" {
		format = "1080"
	} else if quality == "720p60" || quality == "720" {
		format = "720"
	} else if quality == "480" {
		format = "480"
	} else if quality == "360" {
		format = "360"
	} else {
		format = "720"
	}

	for idx, key := range keys {
		maskedKey := "..." + key[len(key)-6:]
		Logger.Info("EXTRACTOR", token, "Trying YouTube MP4/MP3 Downloader with key #%d (%s)", idx+1, maskedKey)

		reqURL := fmt.Sprintf("https://youtube-mp4-mp3-downloader.p.rapidapi.com/api/v1/download?format=%s&id=%s&audioQuality=128&addInfo=false&allowExtendedDuration=false",
			url.QueryEscape(format), url.QueryEscape(videoID))

		req, err := http.NewRequest("GET", reqURL, nil)
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("x-rapidapi-host", "youtube-mp4-mp3-downloader.p.rapidapi.com")
		req.Header.Set("x-rapidapi-key", key)
		req.Header.Set("Accept", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			lastErr = fmt.Errorf("youtube-mp4-mp3-downloader returned HTTP %d", resp.StatusCode)
			continue
		}

		var rawMap map[string]any
		err = json.NewDecoder(resp.Body).Decode(&rawMap)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}

		title := fmt.Sprintf("YouTube Video %s", videoID)
		if t, ok := rawMap["title"].(string); ok && t != "" {
			title = t
		}

		streamURL := ""
		if u, ok := rawMap["url"].(string); ok && u != "" {
			streamURL = u
		} else if l, ok := rawMap["link"].(string); ok && l != "" {
			streamURL = l
		} else if d, ok := rawMap["download_url"].(string); ok && d != "" {
			streamURL = d
		} else if du, ok := rawMap["downloadUrl"].(string); ok && du != "" {
			streamURL = du
		}

		if streamURL != "" {
			Logger.Info("EXTRACTOR", token, "Successfully extracted stream from YouTube MP4/MP3 Downloader!")
			isAudioOnly := (quality == "audio" || quality == "mp3" || quality == "m4a")
			if isAudioOnly {
				return &ExtractedMedia{
					Type:          "audio",
					Title:         title,
					AudioURL:      streamURL,
					AudioLangName: "صوت اصلی",
					HasAudio:      true,
				}, nil
			}

			audioURL := ""
			inURL, _, inErr := ExtractFromInnertube(videoID, audioLang, token)
			if inErr == nil && inURL != "" {
				audioURL = inURL
			}

			return &ExtractedMedia{
				Type:          "video",
				Title:         title,
				VideoURL:      streamURL,
				AudioURL:      audioURL,
				AudioLangName: "پیش‌فرض",
				HasAudio:      audioURL == "",
			}, nil
		}
	}

	return nil, lastErr
}

func extractFromZiyotech(videoID, quality, audioLang string, keys []string, token string) (*ExtractedMedia, error) {
	client := &http.Client{Timeout: 25 * time.Second}
	var lastErr error

	for idx, key := range keys {
		maskedKey := "..." + key[len(key)-6:]
		Logger.Info("EXTRACTOR", token, "Trying Ziyotech Youtube Downloader API with key #%d (%s)", idx+1, maskedKey)

		urlCandidates := []string{
			fmt.Sprintf("https://youtu.be/%s", videoID),
			fmt.Sprintf("https://www.youtube.com/watch?v=%s", videoID),
		}

		var rawVal any
		var lastStatus int

		for _, vURL := range urlCandidates {
			formData := url.Values{}
			formData.Set("url", vURL)
			formData.Set("mode", "auto")

			req, err := http.NewRequest("POST", "https://ziyotech-youtube-downloader-api.p.rapidapi.com/rapid/api/", strings.NewReader(formData.Encode()))
			if err != nil {
				lastErr = err
				continue
			}
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("x-rapidapi-host", "ziyotech-youtube-downloader-api.p.rapidapi.com")
			req.Header.Set("x-rapidapi-key", key)
			req.Header.Set("Accept", "application/json")

			resp, err := client.Do(req)
			if err != nil {
				lastErr = err
				continue
			}
			lastStatus = resp.StatusCode
			if resp.StatusCode == http.StatusOK {
				err = json.NewDecoder(resp.Body).Decode(&rawVal)
				resp.Body.Close()
				if err == nil {
					break
				}
			} else {
				resp.Body.Close()
			}
		}

		if rawVal == nil {
			lastErr = fmt.Errorf("ziyotech api returned HTTP %d", lastStatus)
			continue
		}

		title := fmt.Sprintf("YouTube Video %s", videoID)
		var formatItems []any

		if arr, ok := rawVal.([]any); ok {
			formatItems = arr
		} else if rawMap, ok := rawVal.(map[string]any); ok {
			if t, ok := rawMap["title"].(string); ok && t != "" {
				title = t
			}
			if m, ok := rawMap["media"].([]any); ok {
				formatItems = m
			} else if f, ok := rawMap["formats"].([]any); ok {
				formatItems = f
			} else if dMap, ok := rawMap["data"].(map[string]any); ok {
				if t, ok := dMap["title"].(string); ok && t != "" {
					title = t
				}
				if m, ok := dMap["media"].([]any); ok {
					formatItems = m
				} else if df, ok := dMap["formats"].([]any); ok {
					formatItems = df
				} else if dv, ok := dMap["videos"].([]any); ok {
					formatItems = dv
				}
			} else if d, ok := rawMap["data"].([]any); ok {
				formatItems = d
			} else if v, ok := rawMap["videos"].([]any); ok {
				formatItems = v
			}
		}

		var videoURL, audioURL string
		hasAudio := false

		for _, item := range formatItems {
			im, ok := item.(map[string]any)
			if !ok {
				continue
			}

			u := ""
			if uStr, ok := im["url"].(string); ok {
				u = uStr
			} else if lStr, ok := im["link"].(string); ok {
				u = lStr
			} else if dStr, ok := im["downloadUrl"].(string); ok {
				u = dStr
			}

			if u == "" {
				continue
			}

			qStr := strings.ToLower(fmt.Sprintf("%v", im["quality"]))
			if qStr == "" || qStr == "<nil>" {
				qStr = strings.ToLower(fmt.Sprintf("%v", im["qualityLabel"]))
			}

			mimeStr := strings.ToLower(fmt.Sprintf("%v", im["type"]))
			if mimeStr == "" || mimeStr == "<nil>" {
				mimeStr = strings.ToLower(fmt.Sprintf("%v", im["mimeType"]))
			}

			isAudioStream := strings.Contains(mimeStr, "audio") || strings.Contains(u, "mime=audio") || strings.Contains(qStr, "audio") || strings.Contains(qStr, "mp3")

			if isAudioStream {
				if audioURL == "" {
					audioURL = u
				}
			} else {
				if strings.Contains(qStr, quality) || (quality == "1080" && strings.Contains(qStr, "1080")) || (quality == "720" && strings.Contains(qStr, "720")) {
					videoURL = u
				} else if videoURL == "" {
					videoURL = u
				}
			}
		}

		if videoURL == "" && audioURL == "" {
			if rMap, ok := rawVal.(map[string]any); ok {
				if directURL, ok := rMap["url"].(string); ok && directURL != "" {
					videoURL = directURL
				}
			}
		}

		isAudioOnly := (quality == "audio" || quality == "mp3" || quality == "m4a")
		if isAudioOnly {
			if audioURL == "" {
				audioURL = videoURL
			}
			if audioURL != "" {
				Logger.Info("EXTRACTOR", token, "Successfully extracted audio from Ziyotech API")
				return &ExtractedMedia{
					Type:          "audio",
					Title:         title,
					AudioURL:      audioURL,
					AudioLangName: "صوت اصلی",
					HasAudio:      true,
				}, nil
			}
		}

		if videoURL != "" {
			Logger.Info("EXTRACTOR", token, "Successfully extracted video from Ziyotech API")
			if audioURL == "" && !hasAudio {
				inURL, _, inErr := ExtractFromInnertube(videoID, audioLang, token)
				if inErr == nil && inURL != "" {
					audioURL = inURL
				}
			}
			return &ExtractedMedia{
				Type:          "video",
				Title:         title,
				VideoURL:      videoURL,
				AudioURL:      audioURL,
				AudioLangName: "پیش‌فرض",
				HasAudio:      hasAudio && audioURL == "",
			}, nil
		}
	}

	return nil, lastErr
}

func extractFromYouTubeQuickVideoDownloader(videoID, quality, audioLang string, keys []string, token string) (*ExtractedMedia, error) {
	client := &http.Client{Timeout: 25 * time.Second}
	var lastErr error

	for idx, key := range keys {
		maskedKey := "..." + key[len(key)-6:]
		Logger.Info("EXTRACTOR", token, "Trying YouTube Quick Video Downloader with key #%d (%s)", idx+1, maskedKey)

		vURL := fmt.Sprintf("https://www.youtube.com/watch?v=%s", videoID)
		jsonBody, _ := json.Marshal(map[string]string{"url": vURL})

		req, err := http.NewRequest("POST", "https://youtube-quick-video-downloader.p.rapidapi.com/api/youtube/links", bytes.NewReader(jsonBody))
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-rapidapi-host", "youtube-quick-video-downloader.p.rapidapi.com")
		req.Header.Set("x-rapidapi-key", key)
		req.Header.Set("Accept", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			lastErr = fmt.Errorf("youtube-quick-video-downloader returned HTTP %d", resp.StatusCode)
			continue
		}

		var rawMap map[string]any
		err = json.NewDecoder(resp.Body).Decode(&rawMap)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}

		title := fmt.Sprintf("YouTube Video %s", videoID)
		if t, ok := rawMap["title"].(string); ok && t != "" {
			title = t
		}

		var formatItems []any
		if f, ok := rawMap["formats"].([]any); ok {
			formatItems = f
		} else if l, ok := rawMap["links"].([]any); ok {
			formatItems = l
		} else if d, ok := rawMap["data"].([]any); ok {
			formatItems = d
		} else if v, ok := rawMap["videos"].([]any); ok {
			formatItems = v
		}

		var videoURL, audioURL string
		hasAudio := false

		for _, item := range formatItems {
			im, ok := item.(map[string]any)
			if !ok {
				continue
			}

			u := ""
			if uStr, ok := im["url"].(string); ok {
				u = uStr
			} else if lStr, ok := im["link"].(string); ok {
				u = lStr
			} else if dStr, ok := im["downloadUrl"].(string); ok {
				u = dStr
			}

			if u == "" {
				continue
			}

			qStr := strings.ToLower(fmt.Sprintf("%v", im["quality"]))
			if qStr == "" || qStr == "<nil>" {
				qStr = strings.ToLower(fmt.Sprintf("%v", im["qualityLabel"]))
			}

			mimeStr := strings.ToLower(fmt.Sprintf("%v", im["mimeType"]))
			if mimeStr == "" || mimeStr == "<nil>" {
				mimeStr = strings.ToLower(fmt.Sprintf("%v", im["type"]))
			}

			isAudioStream := strings.Contains(mimeStr, "audio") || strings.Contains(u, "mime=audio") || strings.Contains(qStr, "audio") || strings.Contains(qStr, "mp3")

			if isAudioStream {
				if audioURL == "" {
					audioURL = u
				}
			} else {
				if strings.Contains(qStr, quality) || (quality == "1080" && strings.Contains(qStr, "1080")) || (quality == "720" && strings.Contains(qStr, "720")) {
					videoURL = u
				} else if videoURL == "" {
					videoURL = u
				}
			}
		}

		isAudioOnly := (quality == "audio" || quality == "mp3" || quality == "m4a")
		if isAudioOnly {
			if audioURL == "" {
				audioURL = videoURL
			}
			if audioURL != "" {
				Logger.Info("EXTRACTOR", token, "Successfully extracted audio from YouTube Quick Video Downloader")
				return &ExtractedMedia{
					Type:          "audio",
					Title:         title,
					AudioURL:      audioURL,
					AudioLangName: "صوت اصلی",
					HasAudio:      true,
				}, nil
			}
		}

		if videoURL != "" {
			Logger.Info("EXTRACTOR", token, "Successfully extracted video from YouTube Quick Video Downloader")
			if audioURL == "" && !hasAudio {
				inURL, _, inErr := ExtractFromInnertube(videoID, audioLang, token)
				if inErr == nil && inURL != "" {
					audioURL = inURL
				}
			}
			return &ExtractedMedia{
				Type:          "video",
				Title:         title,
				VideoURL:      videoURL,
				AudioURL:      audioURL,
				AudioLangName: "پیش‌فرض",
				HasAudio:      hasAudio && audioURL == "",
			}, nil
		}
	}

	return nil, lastErr
}

func extractFromYouTube138(videoID, quality, audioLang string, keys []string, token string) (*ExtractedMedia, error) {
	client := &http.Client{Timeout: 25 * time.Second}
	var lastErr error

	for idx, key := range keys {
		maskedKey := "..." + key[len(key)-6:]
		Logger.Info("EXTRACTOR", token, "Trying YouTube138 API with key #%d (%s)", idx+1, maskedKey)

		endpoints := []string{
			fmt.Sprintf("https://youtube138.p.rapidapi.com/video/details/?id=%s&hl=en&gl=US", url.QueryEscape(videoID)),
			fmt.Sprintf("https://youtube138.p.rapidapi.com/video/streaming-data/?id=%s", url.QueryEscape(videoID)),
		}

		var rawMap map[string]any
		var lastStatus int

		for _, reqURL := range endpoints {
			req, err := http.NewRequest("GET", reqURL, nil)
			if err != nil {
				lastErr = err
				continue
			}
			req.Header.Set("x-rapidapi-host", "youtube138.p.rapidapi.com")
			req.Header.Set("x-rapidapi-key", key)
			req.Header.Set("Accept", "application/json")

			resp, err := client.Do(req)
			if err != nil {
				lastErr = err
				continue
			}
			lastStatus = resp.StatusCode
			if resp.StatusCode == http.StatusOK {
				err = json.NewDecoder(resp.Body).Decode(&rawMap)
				resp.Body.Close()
				if err == nil && rawMap != nil {
					break
				}
			} else {
				bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
				resp.Body.Close()
				Logger.Warn("EXTRACTOR", token, "YouTube138 URL (%s) returned HTTP %d: %s", reqURL, resp.StatusCode, string(bodyBytes))
			}
		}

		if rawMap == nil {
			lastErr = fmt.Errorf("youtube138 api returned HTTP %d", lastStatus)
			continue
		}

		title := fmt.Sprintf("YouTube Video %s", videoID)
		if t, ok := rawMap["title"].(string); ok && t != "" {
			title = t
		}

		var formatItems []any
		if af, ok := rawMap["adaptiveFormats"].([]any); ok {
			formatItems = append(formatItems, af...)
		}
		if f, ok := rawMap["formats"].([]any); ok {
			formatItems = append(formatItems, f...)
		}
		if sd, ok := rawMap["streamingData"].(map[string]any); ok {
			if af, ok := sd["adaptiveFormats"].([]any); ok {
				formatItems = append(formatItems, af...)
			}
			if f, ok := sd["formats"].([]any); ok {
				formatItems = append(formatItems, f...)
			}
		}

		var videoURL, audioURL string
		hasAudio := false

		for _, item := range formatItems {
			im, ok := item.(map[string]any)
			if !ok {
				continue
			}
			u := ""
			if uStr, ok := im["url"].(string); ok {
				u = uStr
			}
			if u == "" {
				continue
			}

			qStr := strings.ToLower(fmt.Sprintf("%v", im["qualityLabel"]))
			if qStr == "" || qStr == "<nil>" {
				qStr = strings.ToLower(fmt.Sprintf("%v", im["quality"]))
			}

			mimeStr := strings.ToLower(fmt.Sprintf("%v", im["mimeType"]))
			if mimeStr == "" || mimeStr == "<nil>" {
				mimeStr = strings.ToLower(fmt.Sprintf("%v", im["type"]))
			}

			// فیلتر تصاویر و استوری‌برد
			if strings.Contains(mimeStr, "image") || strings.Contains(mimeStr, "webp") || strings.Contains(u, "storyboard") || strings.Contains(u, "mime=image") || strings.Contains(qStr, "storyboard") {
				continue
			}

			targetHeight := 720
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

			isAudioStream := strings.Contains(mimeStr, "audio") || strings.Contains(u, "mime=audio")

			if isAudioStream {
				if audioURL == "" {
					audioURL = u
				}
			} else {
				if strings.Contains(qStr, strconv.Itoa(targetHeight)) || strings.Contains(qStr, quality) {
					videoURL = u
					hasAudio = strings.Contains(mimeStr, "video") && !strings.Contains(mimeStr, "codecs=\"avc")
				}
			}
		}

		if videoURL == "" && !isAudioOnly {
			lastErr = fmt.Errorf("youtube138 api does not have requested quality %s", quality)
			continue
		}

		isAudioOnly := (quality == "audio" || quality == "mp3" || quality == "m4a")
		if isAudioOnly {
			if audioURL == "" {
				audioURL = videoURL
			}
			if audioURL != "" {
				Logger.Info("EXTRACTOR", token, "Successfully extracted audio from YouTube138 API")
				return &ExtractedMedia{
					Type:          "audio",
					Title:         title,
					AudioURL:      audioURL,
					AudioLangName: "صوت اصلی",
					HasAudio:      true,
				}, nil
			}
		}

		if videoURL != "" {
			Logger.Info("EXTRACTOR", token, "Successfully extracted video from YouTube138 API")
			if audioURL == "" && !hasAudio {
				inURL, _, inErr := ExtractFromInnertube(videoID, audioLang, token)
				if inErr == nil && inURL != "" {
					audioURL = inURL
				}
			}
			return &ExtractedMedia{
				Type:          "video",
				Title:         title,
				VideoURL:      videoURL,
				AudioURL:      audioURL,
				AudioLangName: "پیش‌فرض",
				HasAudio:      hasAudio && audioURL == "",
			}, nil
		}
	}

	return nil, lastErr
}
