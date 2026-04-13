package g711

import "encoding/binary"

const (
	alawMax              = 0x7FFF
	alawToggleMask       = 0x55
	alawSignBit          = 0x80
	alawQuantMask        = 0x0F
	alawSegmentMask      = 0x70
	alawSegmentShift     = 4
	alawSegmentBias      = 0x108
	alawPositiveMask     = 0xD5
	alawLinearThreshold  = 256
	alawSegmentStepShift = 3
	alawMaxSegment       = 8
	pcm16BytesPerSample  = 2
)

var alawSegmentEnds = [...]int{0x1F, 0x3F, 0x7F, 0xFF, 0x1FF, 0x3FF, 0x7FF, 0xFFF}

// DecodeALawToPCM 将 G.711 A-law byte 流解码为 signed 16-bit little-endian PCM。
func DecodeALawToPCM(alaw []byte) []byte {
	pcm := make([]byte, len(alaw)*pcm16BytesPerSample)
	for i, sample := range alaw {
		binary.LittleEndian.PutUint16(pcm[i*pcm16BytesPerSample:], uint16(alawToLinear(sample)))
	}
	return pcm
}

// EncodePCMToALaw 将 signed 16-bit little-endian PCM 编码为 G.711 A-law。
// 如果输入长度为奇数，最后一个无法组成 16-bit 采样的byte会被忽略。
func EncodePCMToALaw(pcm []byte) []byte {
	if len(pcm) < pcm16BytesPerSample {
		return nil
	}
	alaw := make([]byte, len(pcm)/pcm16BytesPerSample)
	for i := range alaw {
		sample := int16(binary.LittleEndian.Uint16(pcm[i*pcm16BytesPerSample:]))
		alaw[i] = linearToALaw(sample)
	}
	return alaw
}

// alawToLinear 按 G.711 A-law 标准把单个 8-bit 样本展开为线性 PCM。
func alawToLinear(aVal byte) int16 {
	aVal ^= alawToggleMask
	t := int16(aVal&alawQuantMask) << alawSegmentShift
	seg := (aVal & alawSegmentMask) >> alawSegmentShift
	switch seg {
	case 0:
		t += alawMaxSegment
	case 1:
		t += alawSegmentBias
	default:
		t += alawSegmentBias
		t <<= seg - 1
	}
	if aVal&alawSignBit != 0 {
		return -t
	}
	return t
}

// linearToALaw 按 G.711 A-law 标准把单个 PCM 样本压缩为 8-bit A-law。
func linearToALaw(sample int16) byte {
	pcm := int(sample)
	mask := byte(alawPositiveMask)
	if pcm < 0 {
		mask = alawToggleMask
		pcm = -pcm - 1
	}
	if pcm > alawMax {
		pcm = alawMax
	}

	var aval byte
	if pcm >= alawLinearThreshold {
		seg := searchSegment(pcm)
		aval = byte(seg << alawSegmentShift)
		aval |= byte((pcm >> (seg + alawSegmentStepShift)) & alawQuantMask)
	} else {
		aval = byte(pcm >> alawSegmentShift)
	}
	return aval ^ mask
}

// searchSegment 查找 A-law 编码时使用的压缩段。
func searchSegment(pcm int) int {
	for seg, end := range alawSegmentEnds {
		if pcm <= end {
			return seg
		}
	}
	return alawMaxSegment
}
