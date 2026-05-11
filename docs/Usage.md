# 使用说明

本文档说明如何在本地启动 `rtc-media-server`，如何配置 realtime WebSocket 串流服务，以及端侧联调时需要发送和接收的最小 JSON 事件。

## 环境要求

- Go toolchain：项目 `go.mod` 声明 `go 1.24.1`，并包含 `toolchain go1.23.5`。
- 配置文件：默认读取 `configs/config.yaml`。
- 端侧测试工具：任意支持 WSS、自签证书跳过校验和自定义 header 的 WebSocket 客户端。

## 启动服务

在项目根目录执行：

```bash
go run ./cmd/rtc-media-server
```

启动流程会：

- 加载 `configs/config.yaml`。
- 初始化 `internal/log` 日志。
- 检查 `websocket.tls.cert_file` 和 `websocket.tls.key_file`；本地开发证书不存在时会自动生成自签证书。
- 启动 WSS 服务并监听 `websocket.listen:websocket.port`。

默认配置下，本地连接地址为：

```text
wss://localhost:8443/v1/realtime
```

由于默认使用自签证书，联调客户端需要信任该证书，或在测试模式下跳过证书校验。

## 运行测试

执行完整测试：

```bash
go test ./...
```

当前测试覆盖配置加载、WebSocket 路由、Session 桥接、pipeline 队列、stage、Controller 和日志模块。

## 关键配置

配置入口是 `configs/config.yaml`。当前最重要的运行项如下：

```yaml
agent:
  protocol: "wss"
  host: "localhost"
  port: 8160
  path: "/v1/ai-edge-agent/omni/session"
  skip_verify: true
  ca_cert_file: ""
  cipher_suites: "TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256, TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384"
  min_version: ""
  image_timeout: 10
  tts_sample_rate: 24000

websocket:
  listen: "0.0.0.0"
  port: 8443
  stream_path: "/v1/realtime"
  client_id_header: "X-Hardware-Id"
  tls:
    cert_file: "configs/certs/dev-server.crt"
    key_file: "configs/certs/dev-server.key"
  rtt_interval: "10s"
  read_timeout: "30s"
  write_timeout: "10s"
  max_message_bytes: 1048576
  stream:
    sample_rate: 8000
    channels: 1

session:
  close_timeout: "3s"
  idle_timeout: "0s"

controller:
  initial_silence_timeout: "15s"
  silence_timeout: "5s"
  reference_queue_size: 16
```

说明：

- `agent.*` 描述 `ServiceConnector` 连接 AI Edge Agent 所需的协议、地址、TLS 和 TTS 采样率配置。
- `websocket.stream_path` 是 realtime WebSocket 通道，默认 `/v1/realtime`。
- `websocket.client_id_header` 默认 `X-Hardware-Id`，服务端用它作为 client/session id。
- `websocket.stream.sample_rate` 和 `websocket.stream.channels` 描述端侧上行音频格式，当前默认 8000 Hz、单声道。
- `controller.reference_queue_size` 控制下行参考信号分发队列长度。

## 建联 Header

客户端连接 `/v1/realtime` 时必须携带：

```text
X-Hardware-Id: <client-id>
```

服务端还会读取以下认证风格 header 并记录非敏感日志，但当前不做签名、时间戳、随机数等完整校验：

```text
X-Auth-Type
X-Product-Key
X-Device-Name
X-Random-Num
X-Timestamp
X-Instance-Id
X-Signature
X-Hardware-Id
```

`X-Signature` 不会打印完整值，只记录是否存在。

## 入站事件

客户端向服务端发送完整 JSON，`type` 字段决定事件类型。

会话更新：

```json
{
  "event_id": "evt-session-1",
  "type": "session.update",
  "session": {
    "modalities": ["audio", "text"]
  }
}
```

追加音频：

```json
{
  "event_id": "evt-audio-1",
  "type": "input_audio_buffer.append",
  "audio": "1VU="
}
```

说明：

- `audio` 是 G.711 A-law bytes 的 base64 文本。
- WebSocket connector 只把 `input_audio_buffer.append.audio` 转成 `media.Frame` 进入上行 pipeline。
- 控制类 JSON 不进入媒体 pipeline，而是转成中性的 `connector.Message` 交给 `Session`。

显式提交输入：

```json
{
  "event_id": "evt-commit-1",
  "type": "input_audio_buffer.commit"
}
```

显式请求回复：

```json
{
  "event_id": "evt-response-1",
  "type": "response.create"
}
```

取消当前回复：

```json
{
  "event_id": "evt-cancel-1",
  "type": "response.cancel"
}
```

## 出站事件

连接成功后，服务端会发送：

```json
{
  "type": "session.created"
}
```

收到 `session.update` 后，服务端会发送：

```json
{
  "type": "session.updated"
}
```

VAD 识别到语音起止后，服务端会发送：

```json
{
  "type": "input_audio_buffer.speech_started"
}
```

```json
{
  "type": "input_audio_buffer.speech_stopped"
}
```

停止后 `Session` 会发送提交确认，并通过 `ServiceConnector` 请求 AI Edge Agent 生成回复：

```json
{
  "type": "input_audio_buffer.committed"
}
```

AI Edge Agent 返回文本 delta、TTS PCM 音频和完成事件后，端侧可能收到：

```json
{
  "type": "response.created"
}
```

```json
{
  "type": "response.audio_transcript.delta",
  "delta": "<text-delta>"
}
```

```json
{
  "type": "response.audio.delta",
  "delta": "<g711-alaw-base64>"
}
```

```json
{
  "type": "response.done"
}
```

错误事件格式：

```json
{
  "type": "error",
  "message": "<error message>"
}
```

## 典型联调流程

1. 客户端携带 `X-Hardware-Id` 连接 `wss://<host>:8443/v1/realtime`。
2. 服务端返回 `session.created`。
3. 客户端发送 `session.update`。
4. 服务端返回 `session.updated`。
5. 客户端发送 `input_audio_buffer.append`，`audio` 字段为 G.711 A-law base64。
6. 上行 pipeline 解码为 PCM16LE，经过 `audio_enhancement(AEC/AGC/ANS)` 和 `vad`。
7. `vad` 触发 `speech_started` 和 `speech_stopped`。
8. `Session` 发送 `input_audio_buffer.committed` 并请求 `ServiceConnector` 创建回复。
9. `ServiceConnector` 将音频流和控制消息转发给 AI Edge Agent。
10. AI Edge Agent 返回文本 delta、TTS PCM 和 done。
11. 下行 pipeline 把 PCM16LE 编码为 G.711 A-law base64。
12. WebSocket connector 发送 `response.audio.delta` 和 `response.done`。

更完整的时序见 [联调时序 Mermaid 图源](uml/stream-sequence.mmd)，也可在 [架构说明](Architecture.md) 中直接查看渲染后的时序图。

## 日志和运行产物

- 日志文件默认写入 `logs/rtc-media-server.log`。
- 日志格式由 `log.format` 控制，支持 `text` 和 `json`。
- `log.level` 支持 `debug`、`info`、`warn`、`error`。
- WebSocket 收到和发出的完整 JSON 使用 debug 日志记录。
- 运行日志会记录连接生命周期、pipeline 错误、控制事件、RTT 和服务侧交互状态。

## 常见问题

- 如果连接返回 400，检查是否缺少 `X-Hardware-Id`。
- 如果连接返回 409，说明相同 `X-Hardware-Id` 的 stream 连接已经存在。
- 如果客户端无法连接 WSS，检查是否信任自签证书，或测试工具是否开启跳过证书校验。
- 如果没有收到 `response.audio.delta`，确认已经发送有效的 `input_audio_buffer.append` 音频，或显式发送 `response.create`。

