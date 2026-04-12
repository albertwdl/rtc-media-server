package stages

import (
	"context"
	"fmt"

	"rtc-media-server/internal/codec/g711"
	"rtc-media-server/internal/media"
)

// ALawDecodeStage 将 G.711 A-law 字节解码为 PCM16LE。
type ALawDecodeStage struct {
	target media.Format
}

func NewALawDecode(target media.Format) *ALawDecodeStage {
	if target.Kind == "" {
		target = media.DefaultPCM16Format()
	}
	return &ALawDecodeStage{target: target}
}

func (s *ALawDecodeStage) Name() string { return "alaw_decode" }

func (s *ALawDecodeStage) Process(ctx context.Context, frame media.Frame) (media.Frame, error) {
	if frame.Format.Codec != media.CodecG711ALaw {
		return frame, fmt.Errorf("alaw decode requires %s, got %s", media.CodecG711ALaw, frame.Format.Codec)
	}
	frame.Payload = g711.DecodeALawToPCM(frame.Payload)
	frame.Format = s.target
	return frame, nil
}

func (s *ALawDecodeStage) Close(ctx context.Context) error { return nil }

// ALawEncodeStage 将 PCM16LE 编码为 G.711 A-law。
type ALawEncodeStage struct{}

func NewALawEncode() *ALawEncodeStage { return &ALawEncodeStage{} }

func (s *ALawEncodeStage) Name() string { return "alaw_encode" }

func (s *ALawEncodeStage) Process(ctx context.Context, frame media.Frame) (media.Frame, error) {
	if frame.Format.Codec != media.CodecPCM16LE {
		return frame, fmt.Errorf("alaw encode requires %s, got %s", media.CodecPCM16LE, frame.Format.Codec)
	}
	frame.Payload = g711.EncodePCMToALaw(frame.Payload)
	frame.Format.Codec = media.CodecG711ALaw
	return frame, nil
}

func (s *ALawEncodeStage) Close(ctx context.Context) error { return nil }
