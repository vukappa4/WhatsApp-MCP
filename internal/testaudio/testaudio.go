// Package testaudio writes audio for tests without ffmpeg, so the tests that
// feed it to ffmpeg need only what the daemon itself uses: they run with any
// ffmpeg build, the minimal one setup-transcription installs on macOS
// included.
package testaudio

import (
	"encoding/binary"
	"math"
	"os"
)

// WAV writes a 16-bit PCM WAV file: seconds long at rate Hz, every one of
// channels channels sampling f at time t (in seconds), clamped to [-1, 1].
func WAV(path string, rate, channels int, seconds float64, f func(t float64) float64) error {
	n := int(math.Round(seconds * float64(rate)))
	data := make([]byte, 0, n*channels*2)
	for i := range n {
		s := uint16(int16(math.Round(max(-1, min(1, f(float64(i)/float64(rate)))) * math.MaxInt16)))
		for range channels {
			data = binary.LittleEndian.AppendUint16(data, s)
		}
	}
	b := make([]byte, 0, 44+len(data))
	b = append(b, "RIFF"...)
	b = binary.LittleEndian.AppendUint32(b, uint32(36+len(data)))
	b = append(b, "WAVEfmt "...)
	b = binary.LittleEndian.AppendUint32(b, 16)
	b = binary.LittleEndian.AppendUint16(b, 1) // PCM
	b = binary.LittleEndian.AppendUint16(b, uint16(channels))
	b = binary.LittleEndian.AppendUint32(b, uint32(rate))
	b = binary.LittleEndian.AppendUint32(b, uint32(rate*channels*2))
	b = binary.LittleEndian.AppendUint16(b, uint16(channels*2))
	b = binary.LittleEndian.AppendUint16(b, 16)
	b = append(b, "data"...)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(data)))
	return os.WriteFile(path, append(b, data...), 0o600)
}

// Tone is a sine of freq Hz at amplitude amp.
func Tone(freq, amp float64) func(t float64) float64 {
	return func(t float64) float64 { return amp * math.Sin(2*math.Pi*freq*t) }
}
