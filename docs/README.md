# rtc-media-server 项目文档

`rtc-media-server` 是端侧 realtime WebSocket 与 AI Edge Agent 之间的媒体接入服务。服务通过 WSS 暴露 `/v1/realtime`，接收端侧 G.711 A-law base64 音频，经过上行 pipeline 转为 PCM16LE 并完成音频增强与 VAD，再通过 `ServiceConnector` 对接 AI Edge Agent；Agent 返回文本 delta、TTS PCM 和完成事件后，下行 pipeline 会把 PCM16LE 编码回 G.711 A-law base64 并发送给端侧。

## 系统能力

- 端侧通过 `wss://<host>:8443/v1/realtime` 建立 realtime WebSocket 连接。
- 使用 `X-Hardware-Id` 作为 client/session id。
- 支持 `input_audio_buffer.append`、`input_audio_buffer.commit`、`session.update`、`response.create`、`response.cancel` 等入站事件。
- 支持 `session.created`、`session.updated`、`input_audio_buffer.speech_started`、`input_audio_buffer.speech_stopped`、`input_audio_buffer.committed`、`response.created`、`response.audio_transcript.delta`、`response.audio.delta`、`response.done`、`error` 等出站事件。
- 每个客户端连接创建独立 `Session`、`Controller`、`ServiceConnector`、上行 pipeline、下行 pipeline 和 stage 实例。
- `ServiceConnector` 对接 AI Edge Agent，承接 ASR、Dialogue、TTS 的音频和控制消息桥接。

## 文档导航

- [使用说明](Usage.md)：本地运行、配置、测试、WSS 联调事件和运行产物。
- [架构说明](Architecture.md)：全局架构、细粒度 Session 架构、数据流、控制流和关闭流程。
- [架构设计与开发规则](Design.md)：当前代码必须遵守的边界、规则和禁止恢复的旧设计残留。
- [全局架构 Mermaid 图源](uml/global-architecture.mmd)：系统级组件关系。
- [Session 细粒度 Mermaid 图源](uml/session-detail.mmd)：单连接内部组件、pipeline 和 stage 顺序。
- [联调时序 Mermaid 图源](uml/stream-sequence.mmd)：典型 realtime WebSocket 串流时序。

## 快速阅读路径

新接手项目时建议按以下顺序阅读：

1. 先读 [使用说明](Usage.md)，确认如何启动、连接和验证最小链路。
2. 再读 [架构说明](Architecture.md)，理解 `websocket`、`session`、`connector`、`pipeline`、`controller` 和 AI Edge Agent 的协作方式。
3. 修改代码前读 [架构设计与开发规则](Design.md)，确认职责边界、日志规则和当前禁止引入的旧抽象。

