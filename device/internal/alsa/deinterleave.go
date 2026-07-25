package alsa

// ExtractS24_3LE copies one channel out of an interleaved S24_3LE buffer into
// out, sign-extending each 24-bit sample to int32. It returns the number of
// frames written.
//
// Callers pass a reusable out slice; nothing is allocated per period.
func ExtractS24_3LE(buf []byte, channels, ch int, out []int32) int {
	frameBytes := channels * 3
	frames := len(buf) / frameBytes
	if frames > len(out) {
		frames = len(out)
	}
	for f := 0; f < frames; f++ {
		o := f*frameBytes + ch*3
		v := int32(buf[o]) | int32(buf[o+1])<<8 | int32(buf[o+2])<<16
		if v&0x800000 != 0 {
			v -= 1 << 24
		}
		out[f] = v
	}
	return frames
}

// MixS24_3LE averages several channels of an interleaved S24_3LE buffer into
// out. With a two-mic device this is the cheapest way to get a single
// omnidirectional feed for wake-word detection without a beamformer.
func MixS24_3LE(buf []byte, channels int, chans []int, out []int32) int {
	if len(chans) == 0 {
		return 0
	}
	frameBytes := channels * 3
	frames := len(buf) / frameBytes
	if frames > len(out) {
		frames = len(out)
	}
	for f := 0; f < frames; f++ {
		var sum int32
		for _, c := range chans {
			o := f*frameBytes + c*3
			v := int32(buf[o]) | int32(buf[o+1])<<8 | int32(buf[o+2])<<16
			if v&0x800000 != 0 {
				v -= 1 << 24
			}
			sum += v
		}
		out[f] = sum / int32(len(chans))
	}
	return frames
}

// DownconvertS24_3LEToS16 converts one channel of an interleaved S24_3LE
// buffer to mono little-endian S16, which is what the wake-word and ASR paths
// upstream expect. It returns the number of bytes written to out.
func DownconvertS24_3LEToS16(buf []byte, channels, ch int, out []byte) int {
	frameBytes := channels * 3
	frames := len(buf) / frameBytes
	if frames*2 > len(out) {
		frames = len(out) / 2
	}
	for f := 0; f < frames; f++ {
		o := f*frameBytes + ch*3
		// Taking the top 16 bits of the 24-bit sample is a plain >>8.
		s := int16(uint16(buf[o+1]) | uint16(buf[o+2])<<8)
		out[f*2] = byte(s)
		out[f*2+1] = byte(s >> 8)
	}
	return frames * 2
}
