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

	// تنظیم مقیاس‌پذیر و پایدار تعداد کانکشن‌های موازی جهت دستیابی به حداکثر سرعت شبکه (۳۰ الی ۵۰+ مگابایت بر ثانیه)
	if contentLength > 4*1024*1024 && acceptRanges {
		numThreads := 12
		if contentLength > 50*1024*1024 {
			numThreads = 32
		} else if contentLength > 15*1024*1024 {
			numThreads = 20
		}
		Logger.Info("DOWNLOADER", token, "Turbo Extreme Multi-Thread Mode ENABLED: %d parallel streams for %.2f MB",
			numThreads, float64(contentLength)/(1024*1024))

		err := downloadMultiThread(ctx, streamURL, destPath, contentLength, numThreads, timeout, token, label, onProgress)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		Logger.Warn("DOWNLOADER", token, "Multi-thread download failed (%v), falling back to single stream", err)
	}

	return downloadSingleStream(ctx, streamURL, destPath, timeout, token, label, onProgress)
}

func downloadMultiThread(ctx context.Context, streamURL, destPath string, totalSize int64, numThreads int, timeout time.Duration, token, label string, onProgress func(written, total int64, speedMBs float64, percent int)) error {
	startTime := time.Now()

	file, err := os.OpenFile(destPath, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0666)
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
			KeepAlive: 60 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          numThreads * 4,
		MaxIdleConnsPerHost:   numThreads * 2,
		MaxConnsPerHost:       numThreads * 2,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   8 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DisableCompression:   true,
		ReadBufferSize:        512 * 1024,
		WriteBufferSize:       512 * 1024,
	}
	defer transport.CloseIdleConnections()

	client := &http.Client{
		Transport: transport,
		Timeout:   timeout,
	}

	stopMonitor := make(chan struct{})
	var wg sync.WaitGroup
	errChan := make(chan error, numThreads)

	// مانیتورینگ زنده و گزارش پیشرفت تجمیعی تمام کانال‌ها به تلگرام
	go func() {
		ticker := time.NewTicker(2500 * time.Millisecond)
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

			var chunkErr error
			for retry := 0; retry < 3; retry++ {
				if ctx.Err() != nil {
					return
				}
				req, err := http.NewRequestWithContext(ctx, "GET", streamURL, nil)
				if err != nil {
					chunkErr = err
					continue
				}
				req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", startByte, endByte))
				req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/122.0.0.0 Safari/537.36")

				resp, err := client.Do(req)
				if err != nil {
					chunkErr = err
					if ctx.Err() != nil {
						return
					}
					time.Sleep(500 * time.Millisecond)
					continue
				}

				if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
					resp.Body.Close()
					chunkErr = fmt.Errorf("bad chunk status: %s", resp.Status)
					time.Sleep(500 * time.Millisecond)
					continue
				}

				buf := make([]byte, 512*1024)
				currentOffset := startByte
				var threadWrittenThisAttempt int64

				for {
					if ctx.Err() != nil {
						resp.Body.Close()
						return
					}
					n, rErr := resp.Body.Read(buf)
					if n > 0 {
						if _, wErr := file.WriteAt(buf[:n], currentOffset); wErr != nil {
							resp.Body.Close()
							chunkErr = wErr
							break
						}
						currentOffset += int64(n)
						threadWrittenThisAttempt += int64(n)
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

				if chunkErr == nil {
					return
				}
				atomic.AddInt64(&totalWritten, -threadWrittenThisAttempt)
				Logger.Warn("DOWNLOADER", token, "Chunk #%d retry %d/3 due to: %v", threadID+1, retry+1, chunkErr)
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
	Logger.Info("DOWNLOADER", token, "Starting single stream download of %s -> %s", label, destPath)

	out, err := os.Create(destPath)
	if err != nil {
		Logger.Error("DOWNLOADER", token, "Failed to create destination file %s: %v", destPath, err)
		return err
	}
	defer out.Close()

	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:   true,
		DisableCompression:  true,
		ReadBufferSize:      128 * 1024,
		WriteBufferSize:     128 * 1024,
	}
	defer transport.CloseIdleConnections()

	client := &http.Client{
		Transport: transport,
		Timeout:   timeout,
	}

	req, err := http.NewRequestWithContext(ctx, "GET", streamURL, nil)
	if err != nil {
		Logger.Error("DOWNLOADER", token, "Failed to build HTTP request for %s: %v", label, err)
		return err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/122.0.0.0 Safari/537.36")

	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		Logger.Error("DOWNLOADER", token, "HTTP request failed for %s after %v: %v", label, time.Since(startTime), err)
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		Logger.Error("DOWNLOADER", token, "Download %s received HTTP %d status: %s", label, resp.StatusCode, resp.Status)
		return fmt.Errorf("bad status: %s", resp.Status)
	}

	contentLength := resp.ContentLength
	if contentLength > 0 {
		Logger.Info("DOWNLOADER", token, "%s Content-Length: %.2f MB", label, float64(contentLength)/(1024*1024))
	} else {
		Logger.Info("DOWNLOADER", token, "%s stream is chunked / dynamic size", label)
	}

	tracker := &ProgressTracker{
		Total:      contentLength,
		StartTime:  startTime,
		LastUpdate: time.Now(),
		OnProgress: onProgress,
	}

	buf := make([]byte, 128*1024)
	written, err := io.CopyBuffer(io.MultiWriter(out, tracker), resp.Body, buf)
	elapsed := time.Since(startTime)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		Logger.Error("DOWNLOADER", token, "Error while streaming %s after %v (written %.2f MB): %v",
			label, elapsed, float64(written)/(1024*1024), err)
		return err
	}

	mbWritten := float64(written) / (1024 * 1024)
	speedMBs := mbWritten / elapsed.Seconds()
	Logger.Info("DOWNLOADER", token, "Successfully downloaded %s: %.2f MB in %v (avg speed: %.2f MB/s)",
		label, mbWritten, elapsed.Round(time.Millisecond), speedMBs)

	return nil
}

func MergeVideoAudio(videoPath, audioPath, outputPath, token string) error {
	startTime := time.Now()
	Logger.Info("FFMPEG", token, "Merging Video (%s) + Audio (%s) -> %s (Mapping stream 0:v:0 + stream 1:a:0)", videoPath, audioPath, outputPath)

	// اولویت اول: کپی مستقیم استریم‌ها با مپینگ دقیق ویدیو از ورودی اول و صدا از ورودی دوم
	cmd := exec.Command("ffmpeg", "-y", "-i", videoPath, "-i", audioPath, "-map", "0:v:0", "-map", "1:a:0", "-c:v", "copy", "-c:a", "aac", "-movflags", "+faststart", outputPath)
	out, err := cmd.CombinedOutput()
	elapsed := time.Since(startTime)

	if err != nil {
		Logger.Warn("FFMPEG", token, "FFmpeg AAC copy-encode failed, retrying with raw copy: %v", err)
		cmd = exec.Command("ffmpeg", "-y", "-i", videoPath, "-i", audioPath, "-map", "0:v:0", "-map", "1:a:0", "-c", "copy", "-movflags", "+faststart", outputPath)
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
