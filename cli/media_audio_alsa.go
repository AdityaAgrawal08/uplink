//go:build linux

package main

import (
	"fmt"

	"github.com/tphakala/go-audio-capture"
)

// openAlsaMic captures via pure-Go ALSA (no cgo, no ffmpeg). Tries exact
// 48kHz mono first, then stereo+downmix, then 44.1kHz+resample — honest
// negotiation, never silent substitution.
func openAlsaMic() (*micCapture, error) {
	devs, err := capture.Devices()
	if err != nil || len(devs) == 0 {
		return nil, fmt.Errorf("no capture devices: %v", err)
	}
	tryConfigs := []capture.Config{
		{Device: devs[0].ID, Rate: voiceRate, Channels: 1, Format: capture.FormatS16LE},
		{Device: devs[0].ID, Rate: voiceRate, Channels: 2, Format: capture.FormatS16LE},
		{Device: devs[0].ID, Rate: 44100, Channels: 1, Format: capture.FormatS16LE},
	}
	var stream *capture.Stream
	var stereo, resample bool
	for i, cfg := range tryConfigs {
		s, err := capture.Open(cfg)
		if err != nil {
			continue
		}
		stream, stereo, resample = s, i == 1, i == 2
		break
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
