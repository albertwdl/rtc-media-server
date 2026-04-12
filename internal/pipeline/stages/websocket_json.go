package stages

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"rtc-media-server/internal/connector"
	"rtc-media-server/internal/media"
	"rtc-media-server/internal/pipeline"
)

const (
	inputAudioAppendEvent   = "input_audio_buffer.append"
	responseAudioDeltaEvent = "response.audio.delta"
)

// EventHandler 用于把 stream JSON 事件上报给 Session。
type EventHandler func(ctx context.Context, frame media.Frame, event connector.Event)

// WebSocketJSONUnpackStage 将 WebSocket stream JSON 解包成后续 stage 可处理的 base64 音频 payload。
type WebSocketJSONUnpackStage struct {
	onEvent EventHandler
}

// NewWebSocketJSONUnpack 创建 WebSocket JSON 解包 stage。
func NewWebSocketJSONUnpack(onEvent EventHandler) *WebSocketJSONUnpackStage {
	return &WebSocketJSONUnpackStage{onEvent: onEvent}
}

// Name 返回 WebSocket JSON 解包 stage 名称。
func (s *WebSocketJSONUnpackStage) Name() string { return "websocket_json_unpack" }

// Process 解析 stream JSON，提取 append 事件中的 base64 音频字段。
func (s *WebSocketJSONUnpackStage) Process(ctx context.Context, frame media.Frame) (media.Frame, error) {
	if frame.Format.Codec != "" && frame.Format.Codec != media.CodecJSON {
		return frame, fmt.Errorf("websocket json unpack requires %s, got %s", media.CodecJSON, frame.Format.Codec)
	}

	var raw struct {
		Type  string `json:"type"`
		Audio string `json:"audio"`
		Delta string `json:"delta"`
	}
	if err := json.Unmarshal(frame.Payload, &raw); err != nil {
		return frame, err
	}

	if s.onEvent != nil {
		s.onEvent(ctx, frame, connector.Event{
			Type: raw.Type,
			Raw:  append([]byte(nil), frame.Payload...),
			Fields: map[string]string{
				"audio": raw.Audio,
				"delta": raw.Delta,
			},
		})
	}

	if raw.Type != inputAudioAppendEvent || raw.Audio == "" {
		return frame, pipeline.ErrDropFrame
	}
	if frame.Metadata == nil {
		frame.Metadata = make(map[string]string)
	}
	frame.Metadata["source_event"] = raw.Type
	frame.Payload = []byte(raw.Audio)
	frame.Format = media.Format{
		Kind:       media.KindAudio,
		Codec:      media.CodecBase64,
		SampleRate: frame.Format.SampleRate,
		Channels:   frame.Format.Channels,
	}
	return frame, nil
}

// Close 关闭 WebSocket JSON 解包 stage。
func (s *WebSocketJSONUnpackStage) Close(ctx context.Context) error { return nil }

// WebSocketJSONPackStage 将 base64 音频 payload 打包成端侧期望的 response.audio.delta JSON。
type WebSocketJSONPackStage struct{}

// NewWebSocketJSONPack 创建 WebSocket JSON 打包 stage。
func NewWebSocketJSONPack() *WebSocketJSONPackStage {
	return &WebSocketJSONPackStage{}
}

// Name 返回 WebSocket JSON 打包 stage 名称。
func (s *WebSocketJSONPackStage) Name() string { return "websocket_json_pack" }

// Process 将 base64 音频 payload 打包为 response.audio.delta JSON。
func (s *WebSocketJSONPackStage) Process(ctx context.Context, frame media.Frame) (media.Frame, error) {
	if frame.Format.Codec != media.CodecBase64 {
		return frame, fmt.Errorf("websocket json pack requires %s, got %s", media.CodecBase64, frame.Format.Codec)
	}
	if len(frame.Payload) == 0 {
		return frame, errors.New("websocket json pack requires non-empty base64 payload")
	}

	payload, err := json.Marshal(struct {
		Type  string `json:"type"`
		Delta string `json:"delta"`
	}{
		Type:  responseAudioDeltaEvent,
		Delta: string(frame.Payload),
	})
	if err != nil {
		return frame, err
	}
	if frame.Metadata == nil {
		frame.Metadata = make(map[string]string)
	}
	frame.Metadata["target_event"] = responseAudioDeltaEvent
	frame.Payload = payload
	frame.Format.Codec = media.CodecJSON
	return frame, nil
}

// Close 关闭 WebSocket JSON 打包 stage。
func (s *WebSocketJSONPackStage) Close(ctx context.Context) error { return nil }
