package protocol

import (
	"encoding/binary"
	"fmt"
)

// MediaCompression names a compression method applied to a cached media
// file's bytes before they're split into DNS blocks.
type MediaCompression byte

const (
	MediaCompressionNone    MediaCompression = 0
	MediaCompressionGzip    MediaCompression = 1
	MediaCompressionDeflate MediaCompression = 2
)

// MediaHeaderVersion is the current header version. Bumped when the layout
// changes incompatibly; until then, the reserved bytes carry future fields.
const MediaHeaderVersion uint8 = 1

// MediaBlockHeaderLen is the fixed length of the metadata prefix that the
// server prepends to a cached media file's bytes before splitting into
// blocks. Block 0 of every media channel begins with these bytes.
//
//	Layout (big-endian where multi-byte):
//	[0:4]   CRC32(IEEE) of the DECOMPRESSED file content
//	[4]     header version (currently 1)
//	[5]     compression byte (MediaCompression*)
//	[6:16]  reserved (zero) — room for future protocol fields without
//	        bumping the version byte
const MediaBlockHeaderLen = 16

// MediaBlockHeader is the parsed form of a media-channel block-0 header.
type MediaBlockHeader struct {
	CRC32       uint32
	Version     uint8
	Compression MediaCompression
}

// EncodeMediaBlockHeader writes the binary header into a fresh slice of
// length MediaBlockHeaderLen. Reserved bytes are zero-padded.
func EncodeMediaBlockHeader(h MediaBlockHeader) []byte {
	buf := make([]byte, MediaBlockHeaderLen)
	binary.BigEndian.PutUint32(buf[0:4], h.CRC32)
	if h.Version == 0 {
		h.Version = MediaHeaderVersion
	}
	buf[4] = h.Version
	buf[5] = byte(h.Compression)
	return buf
}

// DecodeMediaBlockHeader parses the first MediaBlockHeaderLen bytes of a
// media block. Errors on truncation or unknown header version.
func DecodeMediaBlockHeader(b []byte) (MediaBlockHeader, error) {
	if len(b) < MediaBlockHeaderLen {
		return MediaBlockHeader{}, fmt.Errorf("media block header truncated: have %d bytes, need %d", len(b), MediaBlockHeaderLen)
	}
	h := MediaBlockHeader{
		CRC32:       binary.BigEndian.Uint32(b[0:4]),
		Version:     b[4],
		Compression: MediaCompression(b[5]),
	}
	if h.Version != MediaHeaderVersion {
		return MediaBlockHeader{}, fmt.Errorf("media block header version %d not supported (want %d)", h.Version, MediaHeaderVersion)
	}
	switch h.Compression {
	case MediaCompressionNone, MediaCompressionGzip, MediaCompressionDeflate:
	default:
		return MediaBlockHeader{}, fmt.Errorf("media block header: unknown compression %d", h.Compression)
	}
	return h, nil
}

// ParseMediaCompressionName returns the MediaCompression matching one of
// "none", "gzip", "deflate" (case-insensitive). Used by the CLI flag to
// translate user input.
func ParseMediaCompressionName(s string) (MediaCompression, error) {
	switch s {
	case "", "none":
		return MediaCompressionNone, nil
	case "gzip":
		return MediaCompressionGzip, nil
	case "deflate":
		return MediaCompressionDeflate, nil
	}
	return 0, fmt.Errorf("unknown media compression %q", s)
}

// String returns the canonical name of the compression value.
func (c MediaCompression) String() string {
	switch c {
	case MediaCompressionNone:
		return "none"
	case MediaCompressionGzip:
		return "gzip"
	case MediaCompressionDeflate:
		return "deflate"
	}
	return fmt.Sprintf("unknown(%d)", byte(c))
}
