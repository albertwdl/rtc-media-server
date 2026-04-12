package stages

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"

	"rtc-media-server/internal/connector"
	"rtc-media-server/internal/media"
	"rtc-media-server/internal/pipeline"
)

// TestWebSocketJSONUnpackAppend 验证 append 事件会被解包为 base64 音频帧。
func TestWebSocketJSONUnpackAppend(t *testing.T) {
	eventCh := make(chan connector.Event, 1)
	stage := NewWebSocketJSONUnpack(func(ctx context.Context, frame media.Frame, event connector.Event) {
		eventCh <- event
	})
	audio := base64.StdEncoding.EncodeToString([]byte{0xD5})
	raw := []byte(`{"type":"input_audio_buffer.append","audio":"` + audio + `"}`)

	frame, err := stage.Process(context.Background(), media.Frame{
		Payload: raw,
		Format:  media.Format{Kind: media.KindAudio, Codec: media.CodecJSON, SampleRate: 8000, Channels: 1},
	})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if frame.Format.Codec != media.CodecBase64 {
		t.Fatalf("codec = %q", frame.Format.Codec)
	}
	if string(frame.Payload) != audio {
		t.Fatalf("payload = %q, want %q", frame.Payload, audio)
	}
	got := <-eventCh
	if got.Type != "input_audio_buffer.append" {
		t.Fatalf("event type = %q", got.Type)
	}
}

// TestWebSocketJSONUnpackNonAppendDropsFrame 验证非 append 事件会被丢弃。
func TestWebSocketJSONUnpackNonAppendDropsFrame(t *testing.T) {
	stage := NewWebSocketJSONUnpack(nil)
	_, err := stage.Process(context.Background(), media.Frame{
		Payload: []byte(`{"type":"input_audio_buffer.commit"}`),
		Format:  media.Format{Kind: media.KindAudio, Codec: media.CodecJSON},
	})
	if !errors.Is(err, pipeline.ErrDropFrame) {
		t.Fatalf("err = %v, want ErrDropFrame", err)
	}
}

// TestWebSocketJSONPack 验证 base64 音频帧会被打包为下行 JSON。
func TestWebSocketJSONPack(t *testing.T) {
	frame, err := NewWebSocketJSONPack().Process(context.Background(), media.Frame{
		Payload: []byte("1VU="),
		Format:  media.Format{Kind: media.KindAudio, Codec: media.CodecBase64},
	})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if frame.Format.Codec != media.CodecJSON {
		t.Fatalf("codec = %q", frame.Format.Codec)
	}
	var payload struct {
		Type  string `json:"type"`
		Delta string `json:"delta"`
	}
	if err := json.Unmarshal(frame.Payload, &payload); err != nil {
		t.Fatalf("json: %v", err)
	}
	if payload.Type != "response.audio.delta" || payload.Delta != "1VU=" {
		t.Fatalf("payload = %#v", payload)
	}
}
