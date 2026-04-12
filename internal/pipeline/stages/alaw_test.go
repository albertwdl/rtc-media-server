package stages

import (
	"context"
	"encoding/binary"
	"testing"

	"rtc-media-server/internal/media"
)

func TestALawDecodeProducesPCM16(t *testing.T) {
	frame, err := NewALawDecode(media.DefaultPCM16Format()).Process(context.Background(), media.Frame{
		Payload: []byte{0xD5, 0x55},
		Format:  media.Format{Kind: media.KindAudio, Codec: media.CodecG711ALaw},
	})
	if err != nil {
		t.Fatalf("ALawDecode Process: %v", err)
	}
	if frame.Format.Codec != media.CodecPCM16LE {
		t.Fatalf("codec = %q", frame.Format.Codec)
	}
	if len(frame.Payload) != 4 {
		t.Fatalf("payload len = %d, want 4", len(frame.Payload))
	}
	first := int16(binary.LittleEndian.Uint16(frame.Payload[:2]))
	second := int16(binary.LittleEndian.Uint16(frame.Payload[2:]))
	if first != -8 || second != 8 {
		t.Fatalf("samples = %d, %d", first, second)
	}
}

func TestALawEncodeDropsTrailingOddByte(t *testing.T) {
	frame, err := NewALawEncode().Process(context.Background(), media.Frame{
		Payload: []byte{0x01, 0x02, 0x03},
		Format:  media.DefaultPCM16Format(),
	})
	if err != nil {
		t.Fatalf("ALawEncode Process: %v", err)
	}
	if frame.Format.Codec != media.CodecG711ALaw {
		t.Fatalf("codec = %q", frame.Format.Codec)
	}
	if len(frame.Payload) != 1 {
		t.Fatalf("payload len = %d, want 1", len(frame.Payload))
	}
}
