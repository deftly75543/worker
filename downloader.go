package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var chunkBufferPool = sync.Pool{
	New: func() any {
		b := make([]byte, 1024*1024)
		return &b
	},
}

type ProgressTracker struct {
	Total      int64
	Written    int64
	StartTime  time.Time
	LastUpdate time.Time
	LastSpeed  float64
	OnProgress func(written, total int64, speedMBs float64, percent int)
}

func (pt *ProgressTracker) Write(p []byte) (int, error) {
	n := len(p)
	pt.Written += int64(n)

	now := time.Now()
	if pt.OnProgress != nil && (now.Sub(pt.LastUpdate) >= 2000*time.Millisecond || (pt.Total > 0 && pt.Written == pt.Total)) {
		elapsed := now.Sub(pt.StartTime).Seconds()
		if elapsed > 0 {
			pt.LastSpeed = (float64(pt.Written) / (1024 * 1024)) / elapsed
		}
		percent := 0
		if pt.Total > 0 {
			percent = int((float64(pt.Written) / float64(pt.Total)) * 100)
			if percent > 100 {
				percent = 100
			}
		}
		pt.LastUpdate = now
		pt.OnProgress(pt.Written, pt.Total, pt.LastSpeed, percent)
	}
	return n, nil
}

func DownloadStream(ctx context.Context, streamURL, destPath string, timeout time.Duration, token, label string, onProgress func(written, total int64, speedMBs float64, percent int)) error {
	Logger.Info("DOWNLOADER", token, "Initiating high-speed download for %s -> %s", label, destPath)

	// مرحله ۱: استعلام هدرها و سایز فایل
	headClient := &http.Client{Timeout: 10 * time.Second}
	headReq, err := http.NewRequestWithContext(ctx, "HEAD", streamURL, nil)
	if err != nil {
		return downloadSingleStream(ctx, streamURL, destPath, timeout, token, label, onProgress)
	}
	headReq.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/122.0.0.0 Safari/537.36")

	headResp, err := headClient.Do(headReq)
	var contentLength int64
	acceptRanges := false

	if err == nil {
		contentLength = headResp.ContentLength
		if headResp.Header.Get("Accept-Ranges") == "bytes" || headResp.StatusCode == http.StatusOK {
			acceptRanges = true
		}
		headResp.Body.Close()
	}

	// اگر Content-Length از HEAD دریافت نشد، یک درخواست اولیه Range=0-0 ارسال می‌کنیم
	if contentLength <= 0 {
		rangeReq, rErr := http.NewRequestWithContext(ctx, "GET", streamURL, nil)
		if rErr == nil {
			rangeReq.Header.Set("Range", "bytes=0-0")
			rangeReq.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/122.0.0.0 Safari/537.36")
			if rangeResp, rDoErr := headClient.Do(rangeReq); rDoErr == nil {
				if rangeResp.StatusCode == http.StatusPartialContent {
					acceptRanges = true
					cr := rangeResp.Header.Get("Content-Range") // e.g. "bytes 0-0/228414920"
					if idx := strings.LastIndex(cr, "/"); idx != -1 {
						if total, pErr := strconv.ParseInt(cr[idx+1:], 10, 64); pErr == nil && total > 0 {
							contentLength = total
						}
					}
				}
				rangeResp.Body.Close()
			}
		}
	}

	// تنظیم مالتی‌ترد پرسرعت توربو (Turbo Multi-Threading 8-16 parallel streams)
	if contentLength > 5*1024*1024 && acceptRanges {
		numThreads := 8
		if contentLength > 30*1024*1024 {
			numThreads = 12
		}
		if contentLength > 100*1024*1024 {
			numThreads = 16
		}
		Logger.Info("DOWNLOADER", token, "Turbo Multi-Thread Mode ENABLED: %d parallel streams for %.2f MB",
			numThreads, float64(contentLength)/(1024*1024))

		err := downloadMultiThread(ctx, streamURL, destPath, contentLength, numThreads, timeout, token, label, onProgress)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		Logger.Warn("DOWNLOADER", token, "Multi-thread download interrupted (%v), switching to auto-resuming stream", err)
	}

	return downloadSingleStream(ctx, streamURL, destPath, timeout, token, label, onProgress)
}

func downloadMultiThread(ctx context.Context, streamURL, destPath string, totalSize int64, numThreads int, timeout time.Duration, token, label string, onProgress func(written, total int64, speedMBs float64, percent int)) error {
	startTime := time.Now()

	file, err := os.OpenFile(destPath, os.O_CREATE|os.O_RDWR, 0666)
	if err != nil {
		return err
	}
	defer file.Close()

	if err := file.Truncate(totalSize); err != nil {
		Logger.Warn("DOWNLOADER", token, "Could not pre-allocate file size: %v", err)
	}

	var totalWritten int64
	chunkSize := totalSize / int64(numThreads)

	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   8 * time.Second,
			KeepAlive: 90 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          numThreads * 6,
		MaxIdleConnsPerHost:   numThreads * 4,
		MaxConnsPerHost:       numThreads * 4,
		IdleConnTimeout:       120 * time.Second,
		TLSHandshakeTimeout:   8 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		DisableCompression:   true,
		ReadBufferSize:        1024 * 1024,
		WriteBufferSize:       1024 * 1024,
	}
	defer transport.CloseIdleConnections()

	client := &http.Client{
		Transport: transport,
	}

	stopMonitor := make(chan struct{})
	var wg sync.WaitGroup
	errChan := make(chan error, numThreads)

	// مانیتورینگ زنده و گزارش پیشرفت تجمیعی تمام کانال‌ها به تلگرام
	go func() {
		ticker := time.NewTicker(3500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stopMonitor:
				return
			case <-ticker.C:
				currentWritten := atomic.LoadInt64(&totalWritten)
				elapsed := time.Since(startTime).Seconds()
				var speed float64
				if elapsed > 0 {
					speed = (float64(currentWritten) / (1024 * 1024)) / elapsed
				}
				percent := int((float64(currentWritten) / float64(totalSize)) * 100)
				if percent > 100 {
					percent = 100
				}
				if onProgress != nil {
					onProgress(currentWritten, totalSize, speed, percent)
				}
			}
		}
	}()

	for i := 0; i < numThreads; i++ {
		start := int64(i) * chunkSize
		end := start + chunkSize - 1
		if i == numThreads-1 {
			end = totalSize - 1
		}

		wg.Add(1)
		go func(threadID int, startByte, endByte int64) {
			defer wg.Done()

			currentOffset := startByte
			var chunkErr error

			for retry := 0; retry < 5; retry++ {
				if ctx.Err() != nil {
					return
				}
				if currentOffset > endByte {
					return
				}

				reqCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
				req, err := http.NewRequestWithContext(reqCtx, "GET", streamURL, nil)
				if err != nil {
					cancel()
					chunkErr = err
					continue
				}
				req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", currentOffset, endByte))
				req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")
				req.Header.Set("Accept", "*/*")

				resp, err := client.Do(req)
				if err != nil {
					cancel()
					chunkErr = err
					if ctx.Err() != nil {
						return
					}
					time.Sleep(500 * time.Millisecond)
					continue
				}

				if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
					resp.Body.Close()
					cancel()
					chunkErr = fmt.Errorf("bad chunk status: %s", resp.Status)
					time.Sleep(500 * time.Millisecond)
					continue
				}

				bufPtr := chunkBufferPool.Get().(*[]byte)
				buf := *bufPtr

				for {
					if ctx.Err() != nil {
						resp.Body.Close()
						cancel()
						chunkBufferPool.Put(bufPtr)
						return
					}
					n, rErr := resp.Body.Read(buf)
					if n > 0 {
						if _, wErr := file.WriteAt(buf[:n], currentOffset); wErr != nil {
							resp.Body.Close()
							cancel()
							chunkErr = wErr
							break
						}
						currentOffset += int64(n)
						atomic.AddInt64(&totalWritten, int64(n))
					}
					if rErr != nil {
						if rErr == io.EOF {
							chunkErr = nil
						} else {
							chunkErr = rErr
						}
						break
					}
				}
				resp.Body.Close()
				cancel()
				chunkBufferPool.Put(bufPtr)

				if chunkErr == nil && currentOffset > endByte {
					return
				}
				Logger.Warn("DOWNLOADER", token, "Chunk #%d interrupted at offset %d/%d (retry %d/5): %v",
					threadID+1, currentOffset, endByte, retry+1, chunkErr)
				time.Sleep(500 * time.Millisecond)
			}

			if chunkErr != nil && ctx.Err() == nil {
				errChan <- fmt.Errorf("chunk %d failed: %w", threadID, chunkErr)
			}
		}(i, start, end)
	}

	wg.Wait()
	close(stopMonitor)

	if ctx.Err() != nil {
		return ctx.Err()
	}

	select {
	case err := <-errChan:
		return err
	default:
	}

	elapsed := time.Since(startTime)
	mbWritten := float64(atomic.LoadInt64(&totalWritten)) / (1024 * 1024)
	speedMBs := mbWritten / elapsed.Seconds()
	Logger.Info("DOWNLOADER", token, "🚀 Turbo Download COMPLETED for %s: %.2f MB in %v (Aggregated Speed: %.2f MB/s)",
		label, mbWritten, elapsed.Round(time.Millisecond), speedMBs)

	if onProgress != nil {
		onProgress(totalSize, totalSize, speedMBs, 100)
	}
	return nil
}

func downloadSingleStream(ctx context.Context, streamURL, destPath string, timeout time.Duration, token, label string, onProgress func(written, total int64, speedMBs float64, percent int)) error {
	startTime := time.Now()
	Logger.Info("DOWNLOADER", token, "Starting resilient stream download of %s -> %s", label, destPath)

	out, err := os.OpenFile(destPath, os.O_CREATE|os.O_RDWR, 0666)
	if err != nil {
		Logger.Error("DOWNLOADER", token, "Failed to open destination file %s: %v", destPath, err)
		return err
	}
	defer out.Close()

	var totalSize int64 = 0
	var currentWritten int64 = 0

	if fi, statErr := out.Stat(); statErr == nil {
		currentWritten = fi.Size()
	}

	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		DisableCompression:    true,
		ResponseHeaderTimeout: 15 * time.Second,
		ReadBufferSize:        256 * 1024,
		WriteBufferSize:       256 * 1024,
	}
	defer transport.CloseIdleConnections()

	client := &http.Client{
		Transport: transport,
	}

	tracker := &ProgressTracker{
		Total:      totalSize,
		Written:    currentWritten,
		StartTime:  startTime,
		LastUpdate: time.Now(),
		OnProgress: onProgress,
	}

	maxRetries := 6
	for attempt := 1; attempt <= maxRetries; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		reqCtx, cancel := context.WithTimeout(ctx, timeout)
		req, err := http.NewRequestWithContext(reqCtx, "GET", streamURL, nil)
		if err != nil {
			cancel()
			return err
		}
		userAgents := []string{
			"com.google.android.youtube/19.09.37 (Linux; U; Android 11) gzip",
			"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36",
			"com.google.ios.youtube/19.09.3 (iPhone14,3; U; CPU iOS 17_4 like Mac OS X; en_US)",
			"Mozilla/5.0 (Linux; Android 10; K) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Mobile Safari/537.36",
		}
		req.Header.Set("User-Agent", userAgents[(attempt-1)%len(userAgents)])
		req.Header.Set("Accept", "*/*")

		if currentWritten > 0 {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-", currentWritten))
			if _, seekErr := out.Seek(currentWritten, io.SeekStart); seekErr != nil {
				Logger.Warn("DOWNLOADER", token, "Seek error: %v", seekErr)
			}
		}

		resp, err := client.Do(req)
		if err != nil {
			cancel()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			Logger.Warn("DOWNLOADER", token, "Attempt %d/%d connection error: %v, retrying in 1s...", attempt, maxRetries, err)
			time.Sleep(1 * time.Second)
			continue
		}

		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
			resp.Body.Close()
			cancel()
			if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable && currentWritten > 0 {
				// File download completed
				return nil
			}
			Logger.Warn("DOWNLOADER", token, "Attempt %d/%d received status: %s", attempt, maxRetries, resp.Status)
			time.Sleep(1 * time.Second)
			continue
		}

		if totalSize <= 0 {
			if resp.ContentLength > 0 {
				totalSize = currentWritten + resp.ContentLength
				tracker.Total = totalSize
			}
		}

		buf := make([]byte, 256*1024)
		var streamErr error

		for {
			if ctx.Err() != nil {
				resp.Body.Close()
				cancel()
				return ctx.Err()
			}
			n, rErr := resp.Body.Read(buf)
			if n > 0 {
				if _, wErr := out.Write(buf[:n]); wErr != nil {
					streamErr = wErr
					break
				}
				currentWritten += int64(n)
				_, _ = tracker.Write(buf[:n])
			}
			if rErr != nil {
				if rErr == io.EOF {
					streamErr = nil
				} else {
					streamErr = rErr
				}
				break
			}
		}
		resp.Body.Close()
		cancel()

		if streamErr == nil {
			mbWritten := float64(currentWritten) / (1024 * 1024)
			elapsed := time.Since(startTime)
			speedMBs := mbWritten / elapsed.Seconds()
			Logger.Info("DOWNLOADER", token, "Successfully downloaded %s: %.2f MB in %v (avg speed: %.2f MB/s)",
				label, mbWritten, elapsed.Round(time.Millisecond), speedMBs)
			if onProgress != nil && totalSize > 0 {
				onProgress(totalSize, totalSize, speedMBs, 100)
			}
			return nil
		}

		Logger.Warn("DOWNLOADER", token, "Stream stalled/interrupted at %.2f MB (%v), auto-resuming from byte %d (attempt %d/%d)...",
			float64(currentWritten)/(1024*1024), streamErr, currentWritten, attempt, maxRetries)
		time.Sleep(500 * time.Millisecond)
	}

	return fmt.Errorf("download of %s failed after %d auto-resume attempts (downloaded %.2f MB)", label, maxRetries, float64(currentWritten)/(1024*1024))
}

func MergeVideoAudio(videoPath, audioPath, outputPath, token string) error {
	startTime := time.Now()
	Logger.Info("FFMPEG", token, "Merging Video (%s) + Audio (%s) -> %s (Mapping stream 0:v:0 + stream 1:a:0)", videoPath, audioPath, outputPath)

	// اولویت اول: کپی مستقیم استریم‌ها با مپینگ دقیق ویدیو از ورودی اول و صدا از ورودی دوم
	cmd := exec.Command("ffmpeg", "-y", "-threads", "0", "-i", videoPath, "-i", audioPath, "-map", "0:v:0", "-map", "1:a:0", "-c", "copy", "-movflags", "+faststart", outputPath)
	out, err := cmd.CombinedOutput()
	elapsed := time.Since(startTime)

	if err != nil {
		Logger.Warn("FFMPEG", token, "FFmpeg direct copy failed, retrying with AAC audio copy-encode: %v", err)
		cmd = exec.Command("ffmpeg", "-y", "-threads", "0", "-i", videoPath, "-i", audioPath, "-map", "0:v:0", "-map", "1:a:0", "-c:v", "copy", "-c:a", "aac", "-movflags", "+faststart", outputPath)
		out, err = cmd.CombinedOutput()
	}

	if err != nil {
		Logger.Error("FFMPEG", token, "FFmpeg merge FAILED after %v with error: %v. Output:\n%s", elapsed, err, string(out))
		return fmt.Errorf("ffmpeg merge error: %v, output: %s", err, string(out))
	}

	if fi, statErr := os.Stat(outputPath); statErr == nil {
		Logger.Info("FFMPEG", token, "FFmpeg merge completed in %v. Output file size: %.2f MB", elapsed.Round(time.Millisecond), float64(fi.Size())/(1024*1024))
	} else {
		Logger.Info("FFMPEG", token, "FFmpeg merge completed in %v", elapsed.Round(time.Millisecond))
	}

	return nil
}

type VideoMetadata struct {
	Width    int
	Height   int
	Duration int
}

func GetVideoMetadata(videoPath, token string) VideoMetadata {
	meta := VideoMetadata{Width: 1280, Height: 720, Duration: 0}
	cmd := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=width,height,duration:format=duration",
		"-of", "json", videoPath)
	out, err := cmd.Output()
	if err != nil {
		Logger.Warn("METADATA", token, "ffprobe failed (using 1280x720 defaults): %v", err)
		return meta
	}

	var data struct {
		Streams []struct {
			Width    int    `json:"width"`
			Height   int    `json:"height"`
			Duration string `json:"duration"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}

	if err := json.Unmarshal(out, &data); err == nil {
		if len(data.Streams) > 0 {
			if data.Streams[0].Width > 0 {
				meta.Width = data.Streams[0].Width
			}
			if data.Streams[0].Height > 0 {
				meta.Height = data.Streams[0].Height
			}
			if durF, err := strconv.ParseFloat(data.Streams[0].Duration, 64); err == nil && durF > 0 {
				meta.Duration = int(durF)
			}
		}
		if meta.Duration == 0 && data.Format.Duration != "" {
			if durF, err := strconv.ParseFloat(data.Format.Duration, 64); err == nil && durF > 0 {
				meta.Duration = int(durF)
			}
		}
	}

	Logger.Info("METADATA", token, "Detected video dimensions: %dx%d, Duration: %ds", meta.Width, meta.Height, meta.Duration)
	return meta
}

func GenerateThumbnail(videoPath, thumbPath, token string) error {
	startTime := time.Now()
	cmd := exec.Command("ffmpeg", "-y", "-ss", "00:00:01", "-i", videoPath, "-vframes", "1", "-q:v", "2", "-vf", "scale='min(640,iw)':-2", thumbPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		Logger.Warn("THUMBNAIL", token, "Failed to generate thumbnail in %v: %v. Output: %s", time.Since(startTime), err, string(out))
		return err
	}
	Logger.Debug("THUMBNAIL", token, "Generated thumbnail in %v -> %s", time.Since(startTime), thumbPath)
	return nil
}

func TrimMedia(src, dst, startTime, duration, token string) error {
	t0 := time.Now()
	Logger.Info("FFMPEG", token, "Trimming media: Start=%s, Duration=%s -> %s", startTime, duration, dst)
	cmd := exec.Command("ffmpeg", "-y", "-ss", startTime, "-i", src, "-t", duration, "-c", "copy", "-movflags", "+faststart", dst)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// در صورت عدم امکان copy، مجدداً انکود سریع انجام می‌دهیم
		cmd = exec.Command("ffmpeg", "-y", "-ss", startTime, "-i", src, "-t", duration, "-c:v", "libx264", "-preset", "ultrafast", "-c:a", "aac", dst)
		out, err = cmd.CombinedOutput()
	}
	if err != nil {
		Logger.Error("FFMPEG", token, "Trim media failed in %v: %v. Output: %s", time.Since(t0), err, string(out))
		return err
	}
	Logger.Info("FFMPEG", token, "Trim media completed successfully in %v", time.Since(t0))
	return nil
}

func GeneratePreviewGIF(src, dst, token string) error {
	t0 := time.Now()
	Logger.Info("FFMPEG", token, "Generating 4s Preview MP4 Teaser -> %s", dst)
	cmd := exec.Command("ffmpeg", "-y", "-ss", "00:00:04", "-t", "4", "-i", src, "-vf", "fps=12,scale=480:-2:flags=lanczos", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-an", "-movflags", "+faststart", dst)
	out, err := cmd.CombinedOutput()
	if err != nil {
		Logger.Warn("FFMPEG", token, "Preview teaser generation failed in %v: %v. Output: %s", time.Since(t0), err, string(out))
		return err
	}
	Logger.Info("FFMPEG", token, "Preview teaser generated in %v", time.Since(t0))
	return nil
}

func EmbedID3Tags(srcMP3, dstMP3, title, artist, thumbPath, token string) error {
	t0 := time.Now()
	Logger.Info("FFMPEG", token, "Embedding ID3 tags & Album art: Title='%s', Artist='%s'", title, artist)
	var cmd *exec.Cmd
	if thumbPath != "" {
		cmd = exec.Command("ffmpeg", "-y", "-i", srcMP3, "-i", thumbPath, "-map", "0:a", "-map", "1:0", "-c:a", "copy", "-c:v", "copy", "-id3v2_version", "3", "-metadata", "title="+title, "-metadata", "artist="+artist, dstMP3)
	} else {
		cmd = exec.Command("ffmpeg", "-y", "-i", srcMP3, "-c:a", "copy", "-id3v2_version", "3", "-metadata", "title="+title, "-metadata", "artist="+artist, dstMP3)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		Logger.Warn("FFMPEG", token, "ID3 tag embedding failed: %v. Fallback raw file. Output: %s", err, string(out))
		return err
	}
	Logger.Info("FFMPEG", token, "ID3 tags embedded successfully in %v", time.Since(t0))
	return nil
}

func ConvertToVoiceOGG(src, dstOGG, token string) error {
	t0 := time.Now()
	Logger.Info("FFMPEG", token, "Converting audio to Telegram Voice (OGG OPUS) -> %s", dstOGG)
	cmd := exec.Command("ffmpeg", "-y", "-i", src, "-c:a", "libopus", "-b:a", "48k", "-vbr", "on", "-compression_level", "10", dstOGG)
	out, err := cmd.CombinedOutput()
	if err != nil {
		Logger.Error("FFMPEG", token, "Voice conversion failed in %v: %v. Output: %s", time.Since(t0), err, string(out))
		return err
	}
	Logger.Info("FFMPEG", token, "Voice converted successfully in %v", time.Since(t0))
	return nil
}

func MakeRingtone(src, dstMP3, token string) error {
	t0 := time.Now()
	Logger.Info("FFMPEG", token, "Extracting 30s Ringtone -> %s", dstMP3)
	cmd := exec.Command("ffmpeg", "-y", "-ss", "00:00:20", "-t", "30", "-i", src, "-af", "afade=t=in:ss=0:d=1.5,afade=t=out:st=28.5:d=1.5", "-c:a", "libmp3lame", "-b:a", "192k", dstMP3)
	out, err := cmd.CombinedOutput()
	if err != nil {
		Logger.Error("FFMPEG", token, "Ringtone extraction failed: %v. Output: %s", err, string(out))
		return err
	}
	Logger.Info("FFMPEG", token, "Ringtone created in %v", time.Since(t0))
	return nil
}

func CompressVideo(src, dst, token string) error {
	t0 := time.Now()
	Logger.Info("FFMPEG", token, "Data Saver Compression active -> %s", dst)
	cmd := exec.Command("ffmpeg", "-y", "-i", src, "-vf", "scale='min(640,iw)':-2", "-c:v", "libx264", "-crf", "28", "-preset", "veryfast", "-c:a", "aac", "-b:a", "64k", "-movflags", "+faststart", dst)
	out, err := cmd.CombinedOutput()
	if err != nil {
		Logger.Error("FFMPEG", token, "Data saver compression failed: %v. Output: %s", err, string(out))
		return err
	}
	Logger.Info("FFMPEG", token, "Data saver compressed video in %v", time.Since(t0))
	return nil
}

func BurnSubtitle(videoPath, audioPath, subPath, outputPath, token string) error {
	t0 := time.Now()
	Logger.Info("FFMPEG", token, "Burning Hardcoded Subtitles into Video: Video=%s, Audio=%s, Sub=%s -> %s",
		videoPath, audioPath, subPath, outputPath)

	escapedSub := strings.ReplaceAll(subPath, `\`, `/`)
	escapedSub = strings.ReplaceAll(escapedSub, `:`, `\:`)

	subFilter := fmt.Sprintf("subtitles='%s':force_style='FontSize=20,PrimaryColour=&H00FFFFFF,OutlineColour=&H00000000,BorderStyle=3,Outline=2,Shadow=1,MarginV=25'", escapedSub)

	var cmd *exec.Cmd
	if audioPath != "" && audioPath != videoPath {
		cmd = exec.Command("ffmpeg", "-y", "-threads", "0",
			"-i", videoPath,
			"-i", audioPath,
			"-filter_complex", fmt.Sprintf("[0:v]%s[v]", subFilter),
			"-map", "[v]",
			"-map", "1:a:0",
			"-c:v", "libx264",
			"-preset", "ultrafast",
			"-crf", "22",
			"-c:a", "aac",
			"-b:a", "128k",
			"-movflags", "+faststart",
			outputPath,
		)
	} else {
		cmd = exec.Command("ffmpeg", "-y", "-threads", "0",
			"-i", videoPath,
			"-vf", subFilter,
			"-c:v", "libx264",
			"-preset", "ultrafast",
			"-crf", "22",
			"-c:a", "copy",
			"-movflags", "+faststart",
			outputPath,
		)
	}

	out, err := cmd.CombinedOutput()
	elapsed := time.Since(t0)
	if err != nil {
		Logger.Error("FFMPEG", token, "Hardsub burning failed in %v: %v. Output:\n%s", elapsed, err, string(out))
		return fmt.Errorf("hardsub error: %v, output: %s", err, string(out))
	}

	Logger.Info("FFMPEG", token, "Hardcoded subtitles burned successfully in %v -> %s", elapsed.Round(time.Millisecond), outputPath)
	return nil
}

func DownloadWithYtDlp(ctx context.Context, videoURL, quality, audioLang, destPath, token string, onProgress func(percent int, speedMBs float64)) error {
	Logger.Info("DOWNLOADER", token, "Starting direct resilient Yt-Dlp download for %s -> %s", quality, destPath)

	formatSelector := fmt.Sprintf("best[height<=%s]/bestvideo[height<=%s]+bestaudio/best", quality, quality)
	if quality == "audio" || quality == "mp3" || quality == "m4a" {
		formatSelector = "bestaudio/best"
	}

	cmd := exec.CommandContext(ctx, "yt-dlp",
		"--no-warnings",
		"--no-check-certificates",
		"--extractor-args", "youtube:player_client=android,ios,web",
		"-f", formatSelector,
		"-o", destPath,
		videoURL,
	)

	out, err := cmd.CombinedOutput()
	if err != nil {
		Logger.Warn("DOWNLOADER", token, "yt-dlp with android client failed (%v), trying standard fallback mode: %s", err, string(out))
		cmdFallback := exec.CommandContext(ctx, "yt-dlp", "--no-warnings", "-f", formatSelector, "-o", destPath, videoURL)
		if fOut, fErr := cmdFallback.CombinedOutput(); fErr != nil {
			return fmt.Errorf("yt-dlp error: %w | %s", fErr, string(fOut))
		}
	}

	if fi, err := os.Stat(destPath); err == nil && fi.Size() > 1024 {
		Logger.Info("DOWNLOADER", token, "yt-dlp direct download SUCCESS: %.2f MB", float64(fi.Size())/(1024*1024))
		return nil
	}

	return fmt.Errorf("downloaded file not found or empty")
}

