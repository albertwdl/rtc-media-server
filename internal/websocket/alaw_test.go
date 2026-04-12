package websocket

import (
	"encoding/binary"
	"testing"
)

// TestALawRoundTripProducesPCM 验证 A-law 编解码能产生稳定 PCM 输出。
func TestALawRoundTripProducesPCM(t *testing.T) {
	pcm := make([]byte, 8)
	for i, sample := range []int16{-32000, -1000, 1000, 32000} {
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(sample))
	}

	alaw := EncodePCMToALaw(pcm)
	if len(alaw) != 4 {
		t.Fatalf("encoded len = %d", len(alaw))
	}

	decoded := DecodeALawToPCM(alaw)
	if len(decoded) != len(pcm) {
		t.Fatalf("decoded len = %d", len(decoded))
	}
}

// TestDecodeALawToPCMUsesStandardSignBit 验证 A-law 解码符号位符合标准。
func TestDecodeALawToPCMUsesStandardSignBit(t *testing.T) {
	pcm := DecodeALawToPCM([]byte{0xD5, 0x55})
	first := int16(binary.LittleEndian.Uint16(pcm[:2]))
	second := int16(binary.LittleEndian.Uint16(pcm[2:]))
	if first != -8 {
		t.Fatalf("first sample = %d", first)
	}
	if second != 8 {
		t.Fatalf("second sample = %d", second)
	}
}

// TestEncodePCMToALawDropsTrailingOddByte 验证 PCM 奇数字节尾部会被忽略。
func TestEncodePCMToALawDropsTrailingOddByte(t *testing.T) {
	alaw := EncodePCMToALaw([]byte{0x01, 0x02, 0x03})
	if len(alaw) != 1 {
		t.Fatalf("encoded len = %d", len(alaw))
	}
}
