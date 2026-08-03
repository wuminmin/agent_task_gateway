package finalv5oracle

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash"
)

// writeFramed writes one unambiguous length-prefixed field. All independent
// canonical encodings in this package use an unsigned 64-bit big-endian byte
// length followed by the exact field bytes.
func writeFramed(target hash.Hash, value []byte) {
	writeUint64(target, uint64(len(value)))
	_, _ = target.Write(value)
}

func writeUint64(target hash.Hash, value uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	_, _ = target.Write(encoded[:])
}

func digestHex(target hash.Hash) string {
	return hex.EncodeToString(target.Sum(nil))
}

func newDomainHash(domain string) hash.Hash {
	target := sha256.New()
	writeFramed(target, []byte(domain))
	return target
}
