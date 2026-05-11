# 架构说明

本文档描述 `rtc-media-server` 的目标系统架构、单 Session 细粒度架构、媒体数据流和控制消息流。开发边界和实现规则见 [架构设计与开发规则](Design.md)。

## 架构图

- [全局架构 Mermaid 图源](uml/global-architecture.mmd)
- [Session 细粒度 Mermaid 图源](uml/session-detail.mmd)
- [联调时序 Mermaid 图源](uml/stream-sequence.mmd)

这些图使用 Mermaid 编写。下面的 Mermaid 代码块可在支持 Mermaid 的 Markdown 预览中直接渲染，`docs/uml/*.mmd` 是独立图源。

### 全局架构图

```mermaid
flowchart TD
    Client["Realtime WebSocket Client"]
    RuntimeFiles[("logs/")]

    subgraph Process["rtc-media-server"]
        Main["main()"]
        Config["config<br/>internal/config"]
        Log["log<br/>internal/log"]
        WSS["WebSocket Server<br/>internal/websocket.Server"]
        Manager["SessionManager<br/>internal/session.Manager"]

        subgraph PerClient["Per X-Hardware-Id Session"]
            ClientConnector["ClientConnector<br/>websocket.clientConnector"]
            Session["Session<br/>internal/session.Session"]
            Controller["Controller<br/>internal/controller.Controller"]
            Uplink["Uplink Pipeline<br/>internal/pipeline.QueuePipeline"]
            Downlink["Downlink Pipeline<br/>internal/pipeline.QueuePipeline"]
            ServiceConnector["ServiceConnector<br/>internal/connector.ServiceConnector"]
        end
    end

    subgraph Agent["AI Edge Agent"]
        AgentSession["Agent Session"]
        ASR["ASR"]
        Dialogue["Dialogue"]
        TTS["TTS"]
    end

    Client -->|"WSS /v1/realtime<br/>JSON events + G.711 A-law base64 audio"| WSS
    Main -->|"load configs/config.yaml"| Config
    Main -->|"initialize logger"| Log
    WSS -->|"reserve by X-Hardware-Id"| ClientConnector
    WSS -->|"OnConnect(client)"| Manager
    Manager -->|"Attach / NewSession"| Session

    Session -->|"owns"| ClientConnector
    Session -->|"owns"| Controller
    Session -->|"owns"| Uplink
    Session -->|"owns"| Downlink
    Session -->|"owns"| ServiceConnector

    ClientConnector -->|"uplink media.Frame<br/>CodecBase64"| Uplink
    Uplink -->|"PCM16LE audio"| ServiceConnector
    ServiceConnector -->|"audio stream + response control"| AgentSession
    AgentSession --> ASR
    ASR --> Dialogue
    Dialogue --> TTS
    TTS -->|"TTS PCM"| AgentSession
    AgentSession -->|"text delta + TTS PCM + done"| ServiceConnector
    ServiceConnector -->|"downlink PCM16LE"| Downlink
    Downlink -->|"G.711 A-law base64"| ClientConnector
    ClientConnector -->|"response.* JSON"| Client

    Uplink -.->|"VAD StageEvent"| Controller
    Downlink -.->|"reference_tap PCM copy"| Controller
    Controller -.->|"downlink reference for AEC"| Uplink
    WSS -.->|"OnEvent / OnRTT / OnError"| Session

    Log -->|"runtime logs"| RuntimeFiles
```

### Session 细粒度架构图

```mermaid
flowchart LR
    Client["Realtime WebSocket Client"]
    ClientConnector["websocket.clientConnector"]
    Session["session.Session"]
    Controller["controller.Controller"]
    ServiceConnector["connector.ServiceConnector"]

    subgraph Uplink["Uplink Pipeline"]
        Base64Decode["base64_decode"]
        ALawDecode["alaw_decode"]
        AudioEnhancement["audio_enhancement<br/>AEC / AGC / ANS"]
        VAD["vad"]
    end

    subgraph Agent["AI Edge Agent"]
        AgentSession["Agent Session"]
        ASR["ASR"]
        Dialogue["Dialogue"]
        TTS["TTS"]
    end

    subgraph Downlink["Downlink Pipeline"]
        PCMNormalize["pcm_normalize"]
        ReferenceTap["reference_tap"]
        ALawEncode["alaw_encode"]
        Base64Encode["base64_encode"]
    end

    Client -->|"input_audio_buffer.append<br/>audio = G.711 A-law base64"| ClientConnector
    ClientConnector -->|"media.Frame<br/>CodecBase64"| Base64Decode
    Base64Decode -->|"G.711 A-law bytes"| ALawDecode
    ALawDecode -->|"PCM16LE"| AudioEnhancement
    AudioEnhancement -->|"enhanced PCM16LE"| VAD
    VAD -->|"PCM16LE audio"| ServiceConnector

    VAD -.->|"speech_started<br/>speech_stopped<br/>silence_timeout"| Controller
    Controller -.->|"OnStageEvent / CloseSession"| Session
    Session -->|"connector.Message"| ClientConnector
    ClientConnector -->|"session / speech / response JSON"| Client

    Client -->|"session.update<br/>input_audio_buffer.commit<br/>response.create<br/>response.cancel"| ClientConnector
    ClientConnector -->|"connector.Message"| Session
    Session -->|"response control"| ServiceConnector

    ServiceConnector -->|"audio stream + control"| AgentSession
    AgentSession --> ASR
    ASR --> Dialogue
    Dialogue --> TTS
    TTS -->|"TTS PCM"| AgentSession
    AgentSession -->|"text delta + TTS PCM + done"| ServiceConnector

    ServiceConnector -->|"text delta / done"| Session
    ServiceConnector -->|"downlink PCM16LE"| Session
    Session -->|"EnqueueDownlink"| PCMNormalize
    PCMNormalize -->|"normalized PCM16LE"| ReferenceTap
    ReferenceTap -->|"PCM16LE"| ALawEncode
    ALawEncode -->|"G.711 A-law bytes"| Base64Encode
    Base64Encode -->|"CodecBase64"| ClientConnector
    ClientConnector -->|"response.audio.delta"| Client

    ReferenceTap -.->|"OnDownlinkReference"| Controller
    Controller -.->|"AddReference"| AudioEnhancement
    Session -.->|"RTT getter"| AudioEnhancement
```

### 联调时序图

```mermaid
sequenceDiagram
    title realtime WebSocket Stream Sequence
    autonumber
    actor Client as Realtime Client
    participant WS as WebSocket Server
    participant CC as ClientConnector
    participant Manager as SessionManager
    participant Session as Session
    participant Uplink as Uplink Pipeline
    participant Controller as Controller
    participant Service as ServiceConnector
    participant Agent as AI Edge Agent
    participant Downlink as Downlink Pipeline

    Client->>WS: WSS connect /v1/realtime<br/>X-Hardware-Id
    WS->>CC: reserve client and set stream
    WS->>Manager: OnConnect(client)
    Manager->>Session: Attach / NewSession
    Session->>Uplink: Start(base64_decode -> alaw_decode -> audio_enhancement -> vad)
    Session->>Downlink: Start(pcm_normalize -> reference_tap -> alaw_encode -> base64_encode)
    Session->>Service: BindAudioOutput / BindMessageOutput
    Session->>CC: BindAudioOutput / BindMessageOutput
    Service->>Agent: Open agent session
    CC-->>Client: {"type":"session.created"}

    Client->>WS: {"type":"session.update", ...}
    WS->>CC: parse control event
    CC->>Session: MessageSessionUpdated
    CC-->>Client: {"type":"session.updated"}

    Client->>WS: {"type":"input_audio_buffer.append","audio":"..."}
    WS->>CC: extract audio field
    CC->>Uplink: Frame CodecBase64
    Uplink->>Uplink: base64_decode
    Uplink->>Uplink: alaw_decode
    Uplink->>Uplink: audio_enhancement(AEC/AGC/ANS)
    Uplink->>Uplink: vad
    Uplink->>Service: SendAudio PCM16LE
    Service->>Agent: Stream audio PCM16LE

    Uplink->>Controller: speech_started
    Controller->>Session: OnStageEvent
    Session->>CC: MessageSpeechStarted
    CC-->>Client: {"type":"input_audio_buffer.speech_started"}

    Uplink->>Controller: speech_stopped
    Controller->>Session: OnStageEvent
    Session->>CC: MessageSpeechStopped
    CC-->>Client: {"type":"input_audio_buffer.speech_stopped"}
    Session->>CC: MessageInputCommit
    CC-->>Client: {"type":"input_audio_buffer.committed"}
    Session->>Service: MessageResponseCreate
    Service->>Agent: Request response

    Agent-->>Service: response text delta
    Service-->>Session: MessageResponseTextDelta
    Session->>CC: MessageResponseCreate
    CC-->>Client: {"type":"response.created"}
    Session->>CC: MessageResponseTextDelta
    CC-->>Client: {"type":"response.audio_transcript.delta","delta":"..."}

    Agent-->>Service: TTS PCM16LE
    Service-->>Session: PushDownlink PCM16LE
    Session->>Downlink: EnqueueDownlink
    Downlink->>Downlink: pcm_normalize
    Downlink->>Controller: reference_tap PCM copy
    Controller->>Uplink: AddReference to audio_enhancement
    Downlink->>Downlink: alaw_encode
    Downlink->>Downlink: base64_encode
    Downlink->>CC: Frame CodecBase64
    CC-->>Client: {"type":"response.audio.delta","delta":"..."}

    Agent-->>Service: response done
    Service-->>Session: MessageResponseDone
    Session->>CC: MessageResponseDone
    CC-->>Client: {"type":"response.done"}

    loop every websocket.rtt_interval
        WS->>CC: Ping
        CC-->>WS: Pong RTT
        WS->>Session: UpdateRTT
    end
```

## 全局架构

服务入口位于 `cmd/rtc-media-server/main.go`，启动时完成配置加载、日志初始化、TLS 证书准备、`SessionManager` 创建和 `WebSocket Server` 启动。每个端侧 realtime 连接对应一个业务 `Session`，`Session` 通过 `ServiceConnector` 连接 AI Edge Agent。

整体组件关系：

```mermaid
flowchart TD
    Client["Realtime Client"]
    WSS["WebSocket Server"]
    ClientConnector["ClientConnector"]
    Manager["SessionManager"]
    Session["Session"]
    ServiceConnector["ServiceConnector"]
    Agent["AI Edge Agent"]
    Controller["Controller"]
    Uplink["Uplink Pipeline"]
    Downlink["Downlink Pipeline"]

    Client --> WSS
    WSS --> ClientConnector
    ClientConnector --> Manager
    Manager --> Session
    Session --> ServiceConnector
    ServiceConnector --> Agent
    Session --> Controller
    Session --> Uplink
    Session --> Downlink
```

职责分工：

- `internal/websocket` 负责 WSS 接入、端侧 JSON wire event 解析和打包、Ping/Pong RTT 测量。
- `internal/session` 负责每个客户端业务会话的组装、生命周期、上下行 pipeline 桥接和控制消息桥接。
- `internal/connector` 定义客户端连接和服务侧连接的中性接口，不包含端侧 wire event 专用方法。
- `internal/media` 定义媒体帧、格式、方向、pipeline、stage 和 stage event 等媒体模型。
- `internal/pipeline` 提供有界队列 pipeline，每个 stage 拥有独立 goroutine。
- `internal/controller` 负责 VAD 事件、静音关闭、下行参考信号分发等跨管线协调。
- `ServiceConnector` 负责对接 AI Edge Agent，并在 rtc-media-server 与 Agent 之间桥接音频帧、控制消息、文本 delta、TTS PCM 和完成事件。
- `AI Edge Agent` 承载 ASR、Dialogue 和 TTS 能力，对上行音频生成文本、语义回复和下行语音。

## Session 细粒度架构

每个 `X-Hardware-Id` 对应一个客户端连接和一个 `Session`。`SessionManager.Attach` 会为新客户端创建 `Session`，重复连接同一 client id 时复用已有 Session 或由 WebSocket 层拒绝重复 stream。

`Session` 创建时组装以下组件：

- `ClientConnector`：由 WebSocket server 创建，负责端侧连接收发。
- `ServiceConnector`：服务侧连接抽象，负责连接 AI Edge Agent。
- `Controller`：接收 stage 事件和下行 reference tap。
- `Uplink Pipeline`：处理端侧上行音频。
- `Downlink Pipeline`：处理 AI Edge Agent 返回的下行 PCM。

目标 stage chain：

```mermaid
flowchart LR
    subgraph Uplink["uplink"]
        U1["base64_decode"] --> U2["alaw_decode"] --> U3["audio_enhancement<br/>AEC / AGC / ANS"] --> U4["vad"] --> U5["ServiceConnector.SendAudio"]
    end

    subgraph Downlink["downlink"]
        D1["pcm_normalize"] --> D2["reference_tap"] --> D3["alaw_encode"] --> D4["base64_encode"] --> D5["ClientConnector.SendAudio"]
    end
```

stage chain 由 Session 组装，stage 不直接操作 connector。

## 上行媒体流

端侧发送：

```json
{
  "type": "input_audio_buffer.append",
  "audio": "<g711-alaw-base64>"
}
```

处理路径：

```mermaid
flowchart LR
    JSON["WebSocket JSON"]
    Append["clientConnector.handleInputAudioAppend"]
    FrameIn["media.Frame<br/>Direction: uplink<br/>Codec: base64"]
    Base64["base64_decode"]
    ALaw["alaw_decode"]
    FramePCM["media.Frame<br/>Codec: pcm16le"]
    Enhancement["audio_enhancement<br/>AEC / AGC / ANS"]
    VAD["vad"]
    Service["ServiceConnector.SendAudio"]
    Agent["AI Edge Agent"]

    JSON --> Append --> FrameIn --> Base64 --> ALaw --> FramePCM --> Enhancement --> VAD --> Service --> Agent
```

关键规则：

- 只有 `audio` 字段进入媒体 pipeline。
- 上行输入约定为 G.711 A-law base64。
- `base64_decode` 输出 G.711 A-law bytes。
- `alaw_decode` 输出 PCM16LE，默认 8000 Hz、单声道。
- `audio_enhancement` 负责 AEC、AGC、ANS 等上行音频增强能力。
- `vad` 负责语音起止和静音超时事件，事件由 `Controller` 统一仲裁。
- 处理后的 PCM16LE 音频通过 `ServiceConnector` 投递给 AI Edge Agent。

## 下行媒体流

AI Edge Agent 在收到音频流和 response control 后，返回文本 delta、TTS PCM 和完成事件。

处理路径：

```mermaid
flowchart LR
    Agent["AI Edge Agent"]
    Service["ServiceConnector"]
    Enqueue["Session.EnqueueDownlink"]
    Normalize["pcm_normalize"]
    Reference["reference_tap"]
    ALaw["alaw_encode"]
    Base64["base64_encode"]
    SendAudio["ClientConnector.SendAudio"]
    Delta["WebSocket JSON<br/>response.audio.delta"]

    Agent --> Service --> Enqueue --> Normalize --> Reference --> ALaw --> Base64 --> SendAudio --> Delta
```

关键规则：

- `ServiceConnector` 将 Agent 返回的文本 delta 和 done 转为中性的 `connector.Message`。
- `ServiceConnector` 将 Agent 返回的 TTS PCM 投递给 `Session.EnqueueDownlink`。
- `Session.EnqueueDownlink` 会确保本轮回复先发出 `response.created`。
- `pcm_normalize` 把下行 PCM 归一化到目标 PCM16LE 格式。
- `reference_tap` 在编码前复制一份 PCM 给 `Controller`，作为 AEC 参考信号。
- `alaw_encode` 和 `base64_encode` 把 PCM16LE 转成端侧需要的 G.711 A-law base64。

## 控制消息流

WebSocket connector 将端侧控制 JSON 转成中性的 `connector.Message`，由 `Session.OnClientMessage` 处理。

入站控制消息：

```mermaid
flowchart TD
    SessionUpdate["session.update"] --> MessageUpdated["MessageSessionUpdated"] --> OnClient1["Session.OnClientMessage"] --> SessionUpdated["WebSocket sends<br/>session.updated"]

    InputCommit["input_audio_buffer.commit"] --> MessageCommit["MessageInputCommit"] --> OnClient2["Session.OnClientMessage"] --> InputCommitted["WebSocket sends<br/>input_audio_buffer.committed"] --> SendCreate1["ServiceConnector.SendMessage<br/>response_create"] --> Agent1["AI Edge Agent"]

    ResponseCreate["response.create"] --> MessageCreate["MessageResponseCreate"] --> OnClient3["Session.OnClientMessage"] --> SendCreate2["ServiceConnector.SendMessage<br/>response_create"] --> Agent2["AI Edge Agent"]

    ResponseCancel["response.cancel"] --> MessageCancel["MessageResponseCancel"] --> OnClient4["Session.OnClientMessage"] --> SendCancel["ServiceConnector.SendMessage<br/>response_cancel"] --> Agent3["AI Edge Agent"]
```

服务侧消息：

```mermaid
flowchart TD
    TextDelta["AI Edge Agent<br/>text delta"] --> Service1["ServiceConnector"] --> OnService1["Session.OnServiceMessage"] --> ClientSend1["ClientConnector.SendMessage"] --> TranscriptDelta["response.audio_transcript.delta"]

    Done["AI Edge Agent<br/>done"] --> Service2["ServiceConnector"] --> OnService2["Session.OnServiceMessage"] --> ClientSend2["ClientConnector.SendMessage"] --> ResponseDone["response.done"]
```

控制消息不进入媒体 pipeline，端侧 wire event 字符串只属于 `internal/websocket`。

## RTT 与参考信号

WebSocket server 后台按 `websocket.rtt_interval` 对在线连接执行 Ping/Pong：

```mermaid
flowchart LR
    RTTLoop["WebSocket rttLoop"]
    Measure["ClientConnector.MeasureRTT"]
    Callback["callbacks.OnRTT"]
    Update["Session.UpdateRTT"]
    Enhancement["audio_enhancement<br/>Session.RTT getter"]

    RTTLoop --> Measure --> Callback --> Update --> Enhancement
```

RTT 是连接状态，不写入 `media.Frame`，也不通过 frame metadata 传递。

下行参考信号路径：

```mermaid
flowchart LR
    PCM["downlink PCM16LE"]
    Tap["reference_tap"]
    OnReference["Controller.OnDownlinkReference"]
    Dispatch["Controller.dispatchReference"]
    AddReference["audio_enhancement.AddReference"]

    PCM --> Tap --> OnReference --> Dispatch --> AddReference
```

该链路为 AEC 提供下行播放参考，避免上行增强模块直接读取 WebSocket 或下行 connector。

## 生命周期与关闭

连接建立：

1. `WebSocket Server` 校验 `X-Hardware-Id` 并接受 WSS。
2. 为 client id 创建或预留 `clientConnector`。
3. 通过 `OnConnect` 回调调用 `SessionManager.Attach`。
4. `Session` 创建 `Controller`、`ServiceConnector`、上下行 pipeline 和 stage。
5. `ServiceConnector` 建立到 AI Edge Agent 的会话。
6. 绑定 connector 输出端并启动 pipeline。
7. WebSocket connector 发送 `session.created`。

连接断开：

1. WebSocket read loop 退出并 unregister client。
2. `OnDisconnect` 回调调用 `SessionManager.Remove`。
3. `Session.Close` 按固定顺序关闭 `Controller`、上行 pipeline、下行 pipeline、`ServiceConnector`、`ClientConnector`。
4. `ClientConnector` 清理输出端并关闭 `Done` channel。

静音超时：

```mermaid
flowchart LR
    VAD["vad"]
    Emit["Controller.Emit<br/>EventSilenceTimeout"]
    CloseSession["Controller.CloseSession"]
    Close["Session.Close"]

    VAD --> Emit --> CloseSession --> Close
```

静音超时由 VAD stage 产生，关闭流程统一收敛到 `Session.Close`。
