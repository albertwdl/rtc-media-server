package stages

import (
	"context"
	"testing"

	"rtc-media-server/internal/media"
)

// TestBase64StagesRoundTrip 验证 base64 编解码 stage 能往返还原 payload。
func TestBase64StagesRoundTrip(t *testing.T) {
	ctx := context.Background()
	input := media.Frame{
		Payload: []byte{0xD5, 0x55},
		Format:  media.Format{Kind: media.KindAudio, Codec: media.CodecG711ALaw},
	}

	encoded, err := NewBase64Encode().Process(ctx, input)
	if err != nil {
		t.Fatalf("Base64Encode Process: %v", err)
	}
	if encoded.Format.Codec != media.CodecBase64 {
		t.Fatalf("encoded codec = %q", encoded.Format.Codec)
	}

	decoded, err := NewBase64Decode().Process(ctx, encoded)
	if err != nil {
		t.Fatalf("Base64Decode Process: %v", err)
	}
	if decoded.Format.Codec != media.CodecG711ALaw {
		t.Fatalf("decoded codec = %q", decoded.Format.Codec)
	}
	if string(decoded.Payload) != string(input.Payload) {
		t.Fatalf("decoded payload = %#v, want %#v", decoded.Payload, input.Payload)
	}
}
