package pipeline

import (
	"context"
	"encoding/binary"
	"testing"

	"rtc-media-server/internal/media"
)

func TestPCM16NormalizerDownmixesAndResamples(t *testing.T) {
	payload := make([]byte, 8*2)
	for i, sample := range []int16{100, 300, 200, 400, 300, 500, 400, 600} {
		binary.LittleEndian.PutUint16(payload[i*2:], uint16(sample))
	}

	normalizer := NewPCM16Normalizer(media.DefaultPCM16Format())
	frame, err := normalizer.Process(context.Background(), media.Frame{
		Payload: payload,
		Format: media.Format{
			Kind:       media.KindAudio,
			Codec:      media.CodecPCM16LE,
			SampleRate: 16000,
			Channels:   2,
		},
	})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if frame.Format.SampleRate != 8000 || frame.Format.Channels != 1 {
		t.Fatalf("format = %+v", frame.Format)
	}
	if got := len(frame.Payload); got != 4 {
		t.Fatalf("payload bytes = %d", got)
	}
	first := int16(binary.LittleEndian.Uint16(frame.Payload[:2]))
	if first != 200 {
		t.Fatalf("first sample = %d", first)
	}
}
