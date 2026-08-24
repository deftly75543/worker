package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"time"
)

func DownloadStream(streamURL, destPath string, timeout time.Duration, token, label string) error {
	startTime := time.Now()
	Logger.Info("DOWNLOADER", token, "Starting download of %s -> %s", label, destPath)

	out, err := os.Create(destPath)
	if err != nil {
		Logger.Error("DOWNLOADER", token, "Failed to create destination file %s: %v", destPath, err)
		return err
	}
	defer out.Close()

	client := &http.Client{
		Timeout: timeout,
	}

	req, err := http.NewRequest("GET", streamURL, nil)
	if err != nil {
		Logger.Error("DOWNLOADER", token, "Failed to build HTTP request for %s: %v", label, err)
		return err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/122.0.0.0 Safari/537.36")

	resp, err := client.Do(req)
	if err != nil {
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

	written, err := io.Copy(out, resp.Body)
	elapsed := time.Since(startTime)
	if err != nil {
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
	Logger.Info("FFMPEG", token, "Merging Video (%s) + Audio (%s) -> %s", videoPath, audioPath, outputPath)

	cmd := exec.Command("ffmpeg", "-y", "-i", videoPath, "-i", audioPath, "-c", "copy", "-movflags", "+faststart", outputPath)
	out, err := cmd.CombinedOutput()
	elapsed := time.Since(startTime)

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
