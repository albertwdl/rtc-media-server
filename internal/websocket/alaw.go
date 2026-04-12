package websocket

import "rtc-media-server/internal/codec/g711"

// DecodeALawToPCM 将 G.711 A-law 字节流解码为 signed 16-bit little-endian PCM。
// 保留该函数用于兼容旧调用，新 pipeline stage 内部使用 internal/codec/g711。
func DecodeALawToPCM(alaw []byte) []byte {
	return g711.DecodeALawToPCM(alaw)
}

// EncodePCMToALaw 将 signed 16-bit little-endian PCM 编码为 G.711 A-law。
// 保留该函数用于兼容旧调用，新 pipeline stage 内部使用 internal/codec/g711。
func EncodePCMToALaw(pcm []byte) []byte {
	return g711.EncodePCMToALaw(pcm)
}
