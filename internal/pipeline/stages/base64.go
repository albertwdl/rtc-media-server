package stages

import (
	"context"
	"encoding/base64"
	"fmt"

	"rtc-media-server/internal/media"
)

// Base64DecodeStage 将 base64 文本 payload 解码为 G.711 A-law 字节。
type Base64DecodeStage struct{}

// NewBase64Decode 创建 base64 解码 stage。
func NewBase64Decode() *Base64DecodeStage { return &Base64DecodeStage{} }

// Name 返回 base64 解码 stage 名称。
func (s *Base64DecodeStage) Name() string { return "base64_decode" }

// Process 将 base64 payload 解码为二进制 A-law payload。
func (s *Base64DecodeStage) Process(ctx context.Context, frame media.Frame) (media.Frame, error) {
	if frame.Format.Codec != media.CodecBase64 {
		return frame, fmt.Errorf("base64 decode requires %s, got %s", media.CodecBase64, frame.Format.Codec)
	}
	decoded, err := base64.StdEncoding.DecodeString(string(frame.Payload))
	if err != nil {
		return frame, err
	}
	frame.Payload = decoded
	frame.Format.Codec = media.CodecG711ALaw
	return frame, nil
}

// Close 关闭 base64 解码 stage。
func (s *Base64DecodeStage) Close(ctx context.Context) error { return nil }

// Base64EncodeStage 将 G.711 A-law 字节编码为 base64 文本 payload。
type Base64EncodeStage struct{}

// NewBase64Encode 创建 base64 编码 stage。
func NewBase64Encode() *Base64EncodeStage { return &Base64EncodeStage{} }

// Name 返回 base64 编码 stage 名称。
func (s *Base64EncodeStage) Name() string { return "base64_encode" }

// Process 将二进制 A-law payload 编码为 base64 payload。
func (s *Base64EncodeStage) Process(ctx context.Context, frame media.Frame) (media.Frame, error) {
	if frame.Format.Codec != media.CodecG711ALaw {
		return frame, fmt.Errorf("base64 encode requires %s, got %s", media.CodecG711ALaw, frame.Format.Codec)
	}
	frame.Payload = []byte(base64.StdEncoding.EncodeToString(frame.Payload))
	frame.Format.Codec = media.CodecBase64
	return frame, nil
}

// Close 关闭 base64 编码 stage。
func (s *Base64EncodeStage) Close(ctx context.Context) error { return nil }
