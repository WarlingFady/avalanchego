// Copyright (C) 2019-2023, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package paths

import "bytes"

// SerializedPath contains a path from the trie.
// The trie branch factor is 16, so the path may contain an odd number of nibbles.
// If it did contain an odd number of nibbles, the last 4 bits of the last byte should be discarded.
type SerializedPath struct {
	NibbleLength int
	Value        []byte
}

func (s SerializedPath) Equal(other SerializedPath) bool {
	return s.NibbleLength == other.NibbleLength && bytes.Equal(s.Value, other.Value)
}

func (s SerializedPath) Deserialize() TokenPath {
	result := NewTokenPath16(s.Value)
	// trim the last nibble if the path has an odd length
	return result.Slice(0, result.Length()-s.NibbleLength&1)
}

// HasPrefix returns true iff [prefix] is a prefix of [s] or equal to it.
func (s SerializedPath) HasPrefix(prefix SerializedPath) bool {
	prefixValue := prefix.Value
	prefixLength := len(prefix.Value)
	if s.NibbleLength < prefix.NibbleLength || len(s.Value) < prefixLength {
		return false
	}
	if prefix.NibbleLength%2 == 0 {
		return bytes.HasPrefix(s.Value, prefixValue)
	}
	reducedSize := prefixLength - 1

	// the input was invalid so just return false
	if reducedSize < 0 {
		return false
	}

	// grab the last nibble in the prefix and serialized path
	prefixRemainder := prefixValue[reducedSize] >> 4
	valueRemainder := s.Value[reducedSize] >> 4
	// s has prefix if the last nibbles are equal and s has every byte but the last of prefix as a prefix
	return valueRemainder == prefixRemainder && bytes.HasPrefix(s.Value, prefixValue[:reducedSize])
}

// Returns true iff [prefix] is a prefix of [s] but not equal to it.
func (s SerializedPath) HasStrictPrefix(prefix SerializedPath) bool {
	return s.HasPrefix(prefix) && !s.Equal(prefix)
}

func (s SerializedPath) NibbleVal(nibbleIndex int) byte {
	value := s.Value[nibbleIndex>>1]
	isOdd := byte(nibbleIndex & 1)
	isEven := (1 - isOdd)

	// return value first(even index) or last 4(odd index) bits of the corresponding byte
	return isEven*value>>4 + isOdd*(value&0x0F)
}

func (s SerializedPath) AppendNibble(nibble byte) SerializedPath {
	// even is 1 if even, 0 if odd
	even := 1 - s.NibbleLength&1
	value := make([]byte, len(s.Value)+even)
	copy(value, s.Value)

	// shift the nibble 4 left if even, do nothing if odd
	value[len(value)-1] += nibble << (4 * even)
	return SerializedPath{Value: value, NibbleLength: s.NibbleLength + 1}
}
