//go:build linux

package main

import (
	"fmt"

	"github.com/tphakala/go-audio-capture"
)

// openAlsaMic captures via pure-Go ALSA (no cgo, no ffmpeg). Tries exact
// 48kHz mono first, then stereo+downmix, then 44.1kHz+resample — honest
// negotiation, never silent substitution. UPLINK_MIC pins the device;
// otherwise EVERY enumerated capture device is tried in order (devs[0]
// alone was frequently an HDMI/webcam input that opened but produced
// nothing — a top cause of dead audio on Linux).
func openAlsaMic(device string) (*micCapture, error) {
	var tryDevices []capture.DeviceInfo
	if device != "" {
		tryDevices = append(tryDevices, capture.DeviceInfo{ID: device})
	} else {
		devs, err := capture.Devices()
		if err != nil || len(devs) == 0 {
			return nil, fmt.Errorf("no capture devices: %v", err)
		}
		tryDevices = devs
	}
	var stream *capture.Stream
	var stereo, resample bool
	for _, dev := range tryDevices {
		tryConfigs := []capture.Config{
			{Device: dev.ID, Rate: voiceRate, Channels: 1, Format: capture.FormatS16LE},
			{Device: dev.ID, Rate: voiceRate, Channels: 2, Format: capture.FormatS16LE},
			{Device: dev.ID, Rate: 44100, Channels: 1, Format: capture.FormatS16LE},
		}
		found := false
		for i, cfg := range tryConfigs {
			s, err := capture.Open(cfg)
			if err != nil {
				continue
			}
			stream, stereo, resample = s, i == 1, i == 2
			found = true
			break
		}
		if found {
			break
		}
	}
	if stream == nil {
		return nil, fmt.Errorf("no workable ALSA config")
	}
	if err := stream.Start(); err != nil {
		stream.Close()
		return nil, err
	}
	out := make(chan []int16, 50)
	done := make(chan struct{})
	go func() {
		defer close(out)
		defer stream.Close()
		chunker := &frameChunker{}
		buf := make([]byte, 64*1024)
		for {
			select {
			case <-done:
				return
			default:
			}
			n, err := stream.Read(buf)
			if err != nil {
				return
			}
			samples := bytesToS16(buf[:n])
			if stereo {
				samples = downmixStereo(samples)
			}
			if resample {
				samples = resampleLinear(samples, 44100, voiceRate)
			}
			for _, f := range chunker.push(samples) {
				select {
				case out <- f:
				case <-done:
					return
				}
			}
		}
	}()
	return &micCapture{frames: out, stop: func() { close(done) }}, nil
}
