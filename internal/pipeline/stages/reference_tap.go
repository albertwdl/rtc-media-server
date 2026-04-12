package stages

import (
	"context"
	"fmt"

	"rtc-media-server/internal/media"
)

// ReferenceHandler 接收下行参考信号副本。
type ReferenceHandler func(ctx context.Context, frame media.Frame)

// ReferenceTapStage 在下行 PCM 编码前复制一份参考信号给 Controller。
type ReferenceTapStage struct {
	handler ReferenceHandler
}

func NewReferenceTap(handler ReferenceHandler) *ReferenceTapStage {
	return &ReferenceTapStage{handler: handler}
}

func (s *ReferenceTapStage) Name() string { return "reference_tap" }

func (s *ReferenceTapStage) Process(ctx context.Context, frame media.Frame) (media.Frame, error) {
	if frame.Format.Codec != media.CodecPCM16LE {
		return frame, fmt.Errorf("reference tap requires %s, got %s", media.CodecPCM16LE, frame.Format.Codec)
	}
	if s.handler != nil {
		ref := frame
		ref.Payload = append([]byte(nil), frame.Payload...)
		if frame.Metadata != nil {
			ref.Metadata = make(map[string]string, len(frame.Metadata))
			for key, value := range frame.Metadata {
				ref.Metadata[key] = value
			}
		}
		s.handler(ctx, ref)
	}
	return frame, nil
}

func (s *ReferenceTapStage) Close(ctx context.Context) error { return nil }
