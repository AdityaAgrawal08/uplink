//go:build !linux

package main

import "fmt"

// openAlsaMic is Linux-only upstream; other platforms use the ffmpeg
// fallback in openMic.
func openAlsaMic() (*micCapture, error) {
	return nil, fmt.Errorf("ALSA capture is Linux-only")
}
