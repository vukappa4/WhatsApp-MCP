package childproc

import "bytes"

// SniffAudio names the ffmpeg demuxer for an audio container by its magic
// bytes: ogg, wav, mov (MP4, M4A, 3GP), amr, matroska (also WebM), flac,
// mp3 or aac (ADTS); "" for anything else. Callers force that demuxer with
// -f, so a playlist, concat list or SDP file is never probed into one.
func SniffAudio(b []byte) string {
	switch {
	case bytes.HasPrefix(b, []byte("OggS")):
		return "ogg"
	case len(b) >= 12 && bytes.HasPrefix(b, []byte("RIFF")) && string(b[8:12]) == "WAVE":
		return "wav"
	case len(b) >= 12 && string(b[4:8]) == "ftyp":
		return "mov" // ffmpeg's mov,mp4,m4a,3gp demuxer
	case bytes.HasPrefix(b, []byte("#!AMR")):
		return "amr"
	case bytes.HasPrefix(b, []byte{0x1A, 0x45, 0xDF, 0xA3}):
		return "matroska"
	case bytes.HasPrefix(b, []byte("fLaC")):
		return "flac"
	case bytes.HasPrefix(b, []byte("ID3")):
		return "mp3"
	case len(b) >= 2 && b[0] == 0xFF && b[1]&0xF6 == 0xF0:
		return "aac" // ADTS: sync word, layer 00
	case len(b) >= 2 && b[0] == 0xFF && b[1]&0xE0 == 0xE0 && b[1]&0x06 != 0:
		return "mp3" // MPEG audio frame: sync word, layer I-III
	}
	return ""
}
