package stages

import (
	"context"
	"fmt"

	"rtc-media-server/internal/codec/g711"
	"rtc-media-server/internal/media"
)

// ALawDecodeStage 将 G.711 A-law bytes解码为 PCM16LE。
type ALawDecodeStage struct {
	target media.Format
}

// NewALawDecode 创建 G.711 A-law 解码 stage。
func NewALawDecode(target media.Format) *ALawDecodeStage {
	if target.Kind == "" {
		target = media.DefaultPCM16Format()
	}
	return &ALawDecodeStage{target: target}
}

// Name 返回 A-law 解码 stage 名称。
func (s *ALawDecodeStage) Name() string { return "alaw_decode" }

// Process 将 G.711 A-law payload 解码为 PCM16LE。
func (s *ALawDecodeStage) Process(ctx context.Context, frame media.Frame) (media.Frame, error) {
	if frame.Format.Codec != media.CodecG711ALaw {
		return frame, fmt.Errorf("alaw decode requires %s, got %s", media.CodecG711ALaw, frame.Format.Codec)
	}
	frame.Payload = g711.DecodeALawToPCM(frame.Payload)
	frame.Format = s.target
	return frame, nil
}

// Close 关闭 A-law 解码 stage。
func (s *ALawDecodeStage) Close(ctx context.Context) error { return nil }

// ALawEncodeStage 将 PCM16LE 编码为 G.711 A-law。
type ALawEncodeStage struct{}

// NewALawEncode 创建 G.711 A-law 编码 stage。
func NewALawEncode() *ALawEncodeStage { return &ALawEncodeStage{} }

// Name 返回 A-law 编码 stage 名称。
func (s *ALawEncodeStage) Name() string { return "alaw_encode" }

// Process 将 PCM16LE payload 编码为 G.711 A-law。
func (s *ALawEncodeStage) Process(ctx context.Context, frame media.Frame) (media.Frame, error) {
	if frame.Format.Codec != media.CodecPCM16LE {
		return frame, fmt.Errorf("alaw encode requires %s, got %s", media.CodecPCM16LE, frame.Format.Codec)
	}
	frame.Payload = g711.EncodePCMToALaw(frame.Payload)
	frame.Format.Codec = media.CodecG711ALaw
	return frame, nil
}

// Close 关闭 A-law 编码 stage。
func (s *ALawEncodeStage) Close(ctx context.Context) error { return nil }
