// Command generate-secret writes one canonical 256-bit base64url secret for
// operator-directed environment configuration.
package main

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"os"
)

func main() {
	value := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, value); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "secure randomness unavailable")
		os.Exit(1)
	}
	_, _ = fmt.Fprintln(os.Stdout, base64.RawURLEncoding.EncodeToString(value))
}
