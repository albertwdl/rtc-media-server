package g711

import "encoding/binary"

const alawMax = 0x7FFF

// DecodeALawToPCM 将 G.711 A-law 字节流解码为 signed 16-bit little-endian PCM。
func DecodeALawToPCM(alaw []byte) []byte {
	pcm := make([]byte, len(alaw)*2)
	for i, sample := range alaw {
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(alawToLinear(sample)))
	}
	return pcm
}

// EncodePCMToALaw 将 signed 16-bit little-endian PCM 编码为 G.711 A-law。
// 如果输入长度为奇数，最后一个无法组成 16-bit 采样的字节会被忽略。
func EncodePCMToALaw(pcm []byte) []byte {
	if len(pcm) < 2 {
		return nil
	}
	alaw := make([]byte, len(pcm)/2)
	for i := range alaw {
		sample := int16(binary.LittleEndian.Uint16(pcm[i*2:]))
		alaw[i] = linearToALaw(sample)
	}
	return alaw
}

func alawToLinear(aVal byte) int16 {
	aVal ^= 0x55
	t := int16(aVal&0x0F) << 4
	seg := (aVal & 0x70) >> 4
	switch seg {
	case 0:
		t += 8
	case 1:
		t += 0x108
	default:
		t += 0x108
		t <<= seg - 1
	}
	if aVal&0x80 != 0 {
		return -t
	}
	return t
}

func linearToALaw(sample int16) byte {
	pcm := int(sample)
	mask := byte(0xD5)
	if pcm < 0 {
		mask = 0x55
		pcm = -pcm - 1
	}
	if pcm > alawMax {
		pcm = alawMax
	}

	var aval byte
	if pcm >= 256 {
		seg := searchSegment(pcm)
		aval = byte(seg << 4)
		aval |= byte((pcm >> (seg + 3)) & 0x0F)
	} else {
		aval = byte(pcm >> 4)
	}
	return aval ^ mask
}

func searchSegment(pcm int) int {
	for seg, end := range [...]int{0x1F, 0x3F, 0x7F, 0xFF, 0x1FF, 0x3FF, 0x7FF, 0xFFF} {
		if pcm <= end {
			return seg
		}
	}
	return 8
}
