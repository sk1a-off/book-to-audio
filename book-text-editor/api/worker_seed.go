package api

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
)

func randomWorkerSeed(used map[uint32]struct{}) (uint32, error) {
	var encoded [4]byte
	if _, err := io.ReadFull(rand.Reader, encoded[:]); err != nil {
		return 0, fmt.Errorf("read worker seed entropy: %w", err)
	}
	seed := binary.LittleEndian.Uint32(encoded[:])
	for {
		if _, exists := used[seed]; !exists {
			return seed, nil
		}
		seed++
	}
}
