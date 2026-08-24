package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"time"
)

func DownloadStream(streamURL, destPath string, timeout time.Duration) error {
	out, err := os.Create(destPath)
	if err != nil {
		return err
	}
	defer out.Close()

	client := &http.Client{
		Timeout: timeout,
	}

	req, err := http.NewRequest("GET", streamURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/122.0.0.0 Safari/537.36")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bad status: %s", resp.Status)
	}

	_, err = io.Copy(out, resp.Body)
	return err
}

func MergeVideoAudio(videoPath, audioPath, outputPath string) error {
	cmd := exec.Command("ffmpeg", "-y", "-i", videoPath, "-i", audioPath, "-c", "copy", "-movflags", "+faststart", outputPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ffmpeg merge error: %v, output: %s", err, string(out))
	}
	return nil
}

func GenerateThumbnail(videoPath, thumbPath string) error {
	cmd := exec.Command("ffmpeg", "-y", "-ss", "00:00:01", "-i", videoPath, "-vframes", "1", "-q:v", "2", "-vf", "scale='min(640,iw)':-2", thumbPath)
	return cmd.Run()
}
