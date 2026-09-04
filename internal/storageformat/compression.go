package storageformat

import (
	"bytes"
	"compress/gzip"
	"time"
)

// encodeDeterministicGZIP compresses canonical metadata into an in-memory
// buffer. gzip.BestSpeed is a compile-time valid level, and bytes.Buffer writes
// cannot fail, so exposing synthetic I/O failures here would create dead error
// paths rather than a recoverable production condition.
func encodeDeterministicGZIP(magic, canonical []byte) []byte {
	var compressed bytes.Buffer
	compressed.Write(magic)
	writer, _ := gzip.NewWriterLevel(&compressed, gzip.BestSpeed)
	writer.Header.ModTime = time.Unix(0, 0).UTC()
	writer.Header.OS = 255
	_, _ = writer.Write(canonical)
	_ = writer.Close()
	return append([]byte(nil), compressed.Bytes()...)
}
