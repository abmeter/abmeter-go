package abmeter

import (
	"crypto/sha256"
	"encoding/binary"
	"math/bits"
)

// toPercentage maps (salt, id) uniformly onto the integer range 1..100.
//
// This is the cross-SDK bucketing contract: SHA-256 of "{salt}:{id}", the
// first 16 hex characters (i.e. the first 8 bytes, big-endian) as a 64-bit
// integer, then multiply-and-shift into the range. Every language SDK must
// produce identical percentages for the same inputs — locked by the parity
// fixture in testdata/num_utils_parity.json.
func toPercentage(salt, id string) int {
	digest := sha256.Sum256([]byte(salt + ":" + id))
	num := binary.BigEndian.Uint64(digest[:8])
	// (num * 100) >> 64, computed exactly via the 128-bit product's high word.
	hi, _ := bits.Mul64(num, 100)
	return int(hi) + 1
}
