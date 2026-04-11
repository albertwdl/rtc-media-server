package pipeline

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"

	"rtc-media-server/internal/media"
)

// PCM16Normalizer 把 PCM16LE 音频归一化到目标采样率和声道数。
// 当前实现使用轻量的最近邻重采样，便于 WebSocket demo 独立运行；生产可替换为更高质量实现。
type PCM16Normalizer struct {
	target media.Format
}

// NewPCM16Normalizer 创建 PCM16LE 归一化 stage。
func NewPCM16Normalizer(target media.Format) *PCM16Normalizer {
	if target.Kind == "" {
		target = media.DefaultPCM16Format()
	}
	return &PCM16Normalizer{target: target}
}

func (n *PCM16Normalizer) Name() string { return "pcm_normalize" }

func (n *PCM16Normalizer) Process(ctx context.Context, frame media.Frame) (media.Frame, error) {
	if frame.Format.Codec == "" {
		frame.Format.Codec = media.CodecPCM16LE
	}
	if frame.Format.Kind == "" {
		frame.Format.Kind = media.KindAudio
	}
	if frame.Format.Codec != media.CodecPCM16LE {
		return frame, fmt.Errorf("pcm normalizer only supports %s, got %s", media.CodecPCM16LE, frame.Format.Codec)
	}
	if frame.Format.SampleRate <= 0 {
		return frame, errors.New("pcm normalizer requires positive sample rate")
	}
	if frame.Format.Channels <= 0 {
		return frame, errors.New("pcm normalizer requires positive channel count")
	}
	if len(frame.Payload)%2 != 0 {
		return frame, errors.New("pcm16 payload length must be even")
	}

	pcm := bytesToSamples(frame.Payload)
	mono := downmixToMono(pcm, frame.Format.Channels)
	resampled := resampleNearest(mono, frame.Format.SampleRate, n.target.SampleRate)
	frame.Payload = samplesToBytes(resampled)
	frame.Format = n.target
	return frame, nil
}

func (n *PCM16Normalizer) Close(ctx context.Context) error { return nil }

func bytesToSamples(payload []byte) []int16 {
	samples := make([]int16, len(payload)/2)
	for i := range samples {
		samples[i] = int16(binary.LittleEndian.Uint16(payload[i*2:]))
	}
	return samples
}

func samplesToBytes(samples []int16) []byte {
	payload := make([]byte, len(samples)*2)
	for i, sample := range samples {
		binary.LittleEndian.PutUint16(payload[i*2:], uint16(sample))
	}
	return payload
}

func downmixToMono(samples []int16, channels int) []int16 {
	if channels == 1 {
		return samples
	}
	frames := len(samples) / channels
	mono := make([]int16, frames)
	for i := 0; i < frames; i++ {
		var sum int
		for ch := 0; ch < channels; ch++ {
			sum += int(samples[i*channels+ch])
		}
		mono[i] = int16(sum / channels)
	}
	return mono
}

func resampleNearest(samples []int16, fromRate, toRate int) []int16 {
	if fromRate == toRate {
		return samples
	}
	if len(samples) == 0 {
		return nil
	}
	outLen := len(samples) * toRate / fromRate
	if outLen <= 0 {
		outLen = 1
	}
	out := make([]int16, outLen)
	for i := range out {
		src := i * fromRate / toRate
		if src >= len(samples) {
			src = len(samples) - 1
		}
		out[i] = samples[src]
	}
	return out
}
