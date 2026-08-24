package main

import (
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
	Type     string // "video" or "audio"
	Title    string
	VideoURL string
	AudioURL string
	HasAudio bool
}

type RapidAPIVideoItem struct {
	Quality  string `json:"quality"`
	FPS      any    `json:"fps"`
	URL      string `json:"url"`
	HasAudio bool   `json:"hasAudio"`
}

type RapidAPIAudioItem struct {
	Quality string `json:"quality"`
	URL     string `json:"url"`
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


func ExtractFromRapidAPI(rawURL, quality, token string) (*ExtractedMedia, error) {
	videoID := extractVideoID(rawURL)
	if videoID == "" {
		Logger.Error("EXTRACTOR", token, "Invalid YouTube URL provided: %s", rawURL)
		return nil, fmt.Errorf("invalid youtube url: %s", rawURL)
	}

	Logger.Info("EXTRACTOR", token, "Starting extraction for VideoID: %s, Requested Quality: %s", videoID, quality)

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

		Logger.Info("EXTRACTOR", token, "Extracted Title: '%s', Channel: '%s', Duration: %v sec, Videos: %d, Audios: %d",
			title, data.ChannelTitle, data.LengthSeconds, len(data.Videos.Items), len(data.Audios.Items))

		// Best audio stream
		var bestAudioURL string
		if len(data.Audios.Items) > 0 {
			bestAudioURL = data.Audios.Items[0].URL
			Logger.Debug("EXTRACTOR", token, "Found best audio stream quality: %s", data.Audios.Items[0].Quality)
		}

		isAudioOnly := (quality == "audio" || quality == "mp3" || quality == "m4a")
		if isAudioOnly {
			if bestAudioURL != "" {
				Logger.Info("EXTRACTOR", token, "Audio-only requested. Successfully matched audio stream.")
				return &ExtractedMedia{
					Type:     "audio",
					Title:    title,
					AudioURL: bestAudioURL,
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
			audioURL := ""
			if !hasAudio {
				audioURL = bestAudioURL
				Logger.Info("EXTRACTOR", token, "Video stream (%s) is DASH (no embedded audio). Separate audio stream attached for FFmpeg merge.", selectedQuality)
			} else {
				Logger.Info("EXTRACTOR", token, "Video stream (%s) contains embedded audio.", selectedQuality)
			}
			return &ExtractedMedia{
				Type:     "video",
				Title:    title,
				VideoURL: targetVideoURL,
				AudioURL: audioURL,
				HasAudio: hasAudio,
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
