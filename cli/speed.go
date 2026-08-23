package main

import (
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	// minChunk must stay >= 5 MiB: S3/R2 reject CompleteMultipartUpload when
	// any non-final part is smaller than 5 MiB ("minimum allowed object size").
	minChunk     = 5 << 20  // 5 MiB
	maxChunk     = 10 << 20 // 10 MB
	smoothFactor = 0.3      // EMA smoothing factor
	targetSec    = 5.0      // target 5 seconds per chunk
)

type AdaptiveChunker struct {
	measuredSpeed float64 // bytes/sec (EMA)
	samples       int
}

func (ac *AdaptiveChunker) Measure(serverUrl string) (float64, error) {
	speedtestUrl := fmt.Sprintf("%s/api/v1/speedtest", serverUrl)
	start := time.Now()

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(speedtestUrl)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	n, err := io.Copy(io.Discard, resp.Body)
	if err != nil {
		return 0, err
	}

	elapsed := time.Since(start).Seconds()
	if elapsed <= 0 {
		elapsed = 0.001
	}
	speed := float64(n) / elapsed

	ac.RecordSpeed(speed)
	return ac.measuredSpeed, nil
}

func (ac *AdaptiveChunker) RecordSpeed(speed float64) {
	if ac.samples == 0 {
		ac.measuredSpeed = speed
	} else {
		ac.measuredSpeed = smoothFactor*speed + (1-smoothFactor)*ac.measuredSpeed
	}
	ac.samples++
}

func (ac *AdaptiveChunker) ChunkSize() int64 {
	if ac.measuredSpeed <= 0 {
		return 10 * 1024 * 1024 // default 10 MB
	}
	cs := int64(ac.measuredSpeed * targetSec)
	switch {
	case cs < minChunk:
		return minChunk
	case cs > maxChunk:
		return maxChunk
	default:
		return cs
	}
}
