package websocket

import (
	"encoding/binary"
	"testing"
)

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

func TestEncodePCMToALawDropsTrailingOddByte(t *testing.T) {
	alaw := EncodePCMToALaw([]byte{0x01, 0x02, 0x03})
	if len(alaw) != 1 {
		t.Fatalf("encoded len = %d", len(alaw))
	}
}
