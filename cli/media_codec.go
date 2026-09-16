package main

import (
	"encoding/binary"
	"fmt"
)

// ─── Media payload codecs (inside the Noise datagram) ───────────────────────
//
// Audio frames carry their own sequence + timestamp so the jitter buffer can
// order them across loss and reorder: [seq:2][ts:4][opus]. Video fragments:
// [seq:2][fragIdx:2][fragTotal:2][ts:4][data]. All fields big-endian.

const (
	audioHeaderLen     = 2 + 4
	videoFragHeaderLen = 2 + 2 + 2 + 4
)

type audioPacket struct {
	Seq  uint16
	Ts   uint32
	Opus []byte
}

func encodeAudioPacket(seq uint16, ts uint32, opus []byte) []byte {
	out := make([]byte, 0, audioHeaderLen+len(opus))
	var hb [6]byte
	binary.BigEndian.PutUint16(hb[0:2], seq)
	binary.BigEndian.PutUint32(hb[2:6], ts)
	out = append(out, hb[:]...)
	return append(out, opus...)
}

func decodeAudioPacket(raw []byte) (audioPacket, error) {
	if len(raw) < audioHeaderLen {
		return audioPacket{}, fmt.Errorf("short audio packet")
	}
	return audioPacket{
		Seq:  binary.BigEndian.Uint16(raw[0:2]),
		Ts:   binary.BigEndian.Uint32(raw[2:6]),
		Opus: raw[6:],
	}, nil
}

type videoFrag struct {
	Seq       uint16
	FragIdx   uint16
	FragTotal uint16
	Ts        uint32
	Data      []byte
}

func encodeVideoFrag(seq, idx, total uint16, ts uint32, data []byte) []byte {
	out := make([]byte, 0, videoFragHeaderLen+len(data))
	var hb [10]byte
	binary.BigEndian.PutUint16(hb[0:2], seq)
	binary.BigEndian.PutUint16(hb[2:4], idx)
	binary.BigEndian.PutUint16(hb[4:6], total)
	binary.BigEndian.PutUint32(hb[6:10], ts)
	out = append(out, hb[:]...)
	return append(out, data...)
}

func decodeVideoFrag(raw []byte) (videoFrag, error) {
	if len(raw) < videoFragHeaderLen {
		return videoFrag{}, fmt.Errorf("short video frag")
	}
	return videoFrag{
		Seq:       binary.BigEndian.Uint16(raw[0:2]),
		FragIdx:   binary.BigEndian.Uint16(raw[2:4]),
		FragTotal: binary.BigEndian.Uint16(raw[4:6]),
		Ts:        binary.BigEndian.Uint32(raw[6:10]),
		Data:      raw[10:],
	}, nil
}

func (m *mediaTransport) dispatchMedia(peer string, kind byte, pt []byte) {
	switch kind {
	case mediaKindAudio:
		pkt, err := decodeAudioPacket(pt)
		if err != nil || m.cb.onAudio == nil {
			return
		}
		m.cb.onAudio(peer, pkt)
	case mediaKindVideo:
		frag, err := decodeVideoFrag(pt)
		if err != nil || m.cb.onVideo == nil {
			return
		}
		if frag.FragTotal == 0 || frag.FragIdx >= frag.FragTotal {
			return
		}
		m.cb.onVideo(peer, frag)
	case mediaKindKeyReq:
		if m.cb.onKeyReq != nil {
			m.cb.onKeyReq(peer)
		}
	case mediaKindPing:
		_ = m.sendMedia(peer, mediaKindPong, nil)
	case mediaKindPong:
	default:
	}
}
