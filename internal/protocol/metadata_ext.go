package protocol

import (
	"crypto/sha256"
	"fmt"
)

// Extended metadata header packs block_count + content_hash into the
// Marker[3] and Timestamp[4] fields of V0 metadata. Those fields are
// serialized + deserialized but never read by clients, so old clients keep
// working with no schema change while new clients use the embedded header
// to fetch all metadata blocks in parallel and validate the assembled
// payload was not mixed across server snapshots.
//
// Wire layout (occupies the first 7 bytes of V0-serialized metadata, in
// the slots Marker[3] and Timestamp[4]):
//
//	[0]    EMHMagic0    = 0xFE
//	[1]    EMHMagic1    = 0xED
//	[2]    flags        uint8 — reserved (see EMHFlag*)
//	[3]    block_count  uint8 (1..255 total metadata blocks)
//	[4..6] content_hash 3 bytes — first 3 bytes of SHA-256(payload[7:])
//
// The hash covers the body only — everything after the 7-byte header —
// so it changes whenever the actual metadata content changes (channel
// list, NextFetch, flags, etc.) but is independent of the block split.
// Magic bytes. 2 bytes of magic → 1/65536 probability that an old server's
// random 3-byte Marker happens to start with FE ED. False-positive cost is
// bounded: hash verify fails → retry → eventually cooldown for 10 min and
// fall back to the legacy fetch path. Acceptable; reduce by adding a third
// magic byte if we ever observe this misbehaviour in the wild.
const (
	EMHMagic0    byte = 0xFE
	EMHMagic1    byte = 0xED
	EMHHeaderLen      = MarkerSize + 4 // 3 (Marker) + 4 (Timestamp) = 7
	EMHHashLen        = 3
	EMHMaxBlocks      = 255

	// EMHFlagNewerAvailable, when bit-set in the flags byte at offset [2],
	// signals "a newer metadata format exists; if you support it, switch".
	// Reserved for future use — current encoders never set it; current
	// decoders never read it. Bits 1..7 are reserved for future flags.
	EMHFlagNewerAvailable byte = 1 << 0
)

// EncodeMetadataExtended serializes metadata with the extended header
// embedded in Marker/Timestamp, splits into blocks, and patches the final
// block_count into block 0. Callers feed plain *Metadata; the Marker and
// Timestamp fields on the input are overwritten with the embedded header.
func EncodeMetadataExtended(m *Metadata) ([][]byte, error) {
	tmp := *m
	// Flags byte (Marker[2]) is left at 0 — no upgrade signal in this rev.
	tmp.Marker = [MarkerSize]byte{EMHMagic0, EMHMagic1, 0}
	tmp.Timestamp = 0 // placeholder; patched after split

	raw := SerializeMetadata(&tmp)
	if len(raw) < EMHHeaderLen {
		return nil, fmt.Errorf("EMH: serialized metadata too short: %d bytes", len(raw))
	}

	// Hash the body (everything after the 7-byte header).
	sum := sha256.Sum256(raw[EMHHeaderLen:])
	raw[4] = sum[0]
	raw[5] = sum[1]
	raw[6] = sum[2]

	blocks := SplitIntoBlocks(raw)
	if len(blocks) == 0 {
		return nil, fmt.Errorf("EMH: SplitIntoBlocks returned 0 blocks")
	}
	if len(blocks) > EMHMaxBlocks {
		return nil, fmt.Errorf("EMH: too many blocks: %d > %d", len(blocks), EMHMaxBlocks)
	}
	if len(blocks[0]) < EMHHeaderLen {
		return nil, fmt.Errorf("EMH: block 0 too short to hold header: %d < %d", len(blocks[0]), EMHHeaderLen)
	}
	blocks[0][3] = uint8(len(blocks))
	return blocks, nil
}

// PeekExtendedHeader inspects the first EMHHeaderLen bytes of a metadata
// block 0. Returns (extended=true, count, hash, nil) when the magic +
// version match; (extended=false, …, nil) when this is a legacy V0
// payload from an old server. Errors are reserved for genuinely malformed
// input (block too short or block_count=0 with magic present).
func PeekExtendedHeader(block0 []byte) (extended bool, blockCount uint8, hash [EMHHashLen]byte, err error) {
	if len(block0) < EMHHeaderLen {
		return false, 0, hash, fmt.Errorf("EMH: block 0 too short: %d bytes", len(block0))
	}
	if block0[0] != EMHMagic0 || block0[1] != EMHMagic1 {
		return false, 0, hash, nil
	}
	// block0[2] is the flags byte. Any value is currently a valid header —
	// flags are informational only (see EMHFlag*) and don't gate parsing.
	blockCount = block0[3]
	hash[0] = block0[4]
	hash[1] = block0[5]
	hash[2] = block0[6]
	if blockCount == 0 {
		return true, 0, hash, fmt.Errorf("EMH: block_count is 0")
	}
	return true, blockCount, hash, nil
}

// VerifyExtendedHash returns nil when the first 3 bytes of SHA-256(body)
// match the hash embedded in the header. body must be the assembled bytes
// AFTER the 7-byte EMH header.
func VerifyExtendedHash(hash [EMHHashLen]byte, body []byte) error {
	sum := sha256.Sum256(body)
	if sum[0] != hash[0] || sum[1] != hash[1] || sum[2] != hash[2] {
		return fmt.Errorf("EMH hash mismatch (snapshot drift?)")
	}
	return nil
}
