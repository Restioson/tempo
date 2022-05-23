package io

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBufferedReaderAtCalculateBounds(t *testing.T) {

	testCases := []struct {
		readerAtSize                   int64
		offset, length                 int64
		bufferSize                     int
		expectedOffset, expectedLength int64
	}{
		// Happy case - read in middle of file is extended
		{
			100,   // Input is 100 bytes long
			1, 50, // Read 50 bytes
			75, // Buffer attempts 75
			1, 75,
		},

		// Request is larger than buffer size
		{
			100,   // Input is 100 bytes long
			1, 50, // Read 50 bytes
			25, // Buffer is only 25
			1, 50,
		},

		// ReaderAt size smaller than the buffer size
		{
			100,    // Input is 100 bytes long
			0, 100, // Read 100 bytes at beginning
			1000, // Buffer attempts 1000
			0, 100,
		},

		// Read at end of file is backed up to buffer size
		{
			100,   // Input is 100 bytes long
			99, 1, // Read last byte
			10,     // Buffer attempts 10
			90, 10, // Offset backed up to satisfy buffer size
		},

		// No buffering
		{
			100,    // Input is 100 bytes long
			25, 50, // Read 50 bytes in the middle
			0, // No buffering
			25, 50,
		},
	}

	for _, tc := range testCases {
		o, l := calculateBounds(tc.offset, tc.length, tc.bufferSize, tc.readerAtSize)
		require.Equal(t, tc.expectedOffset, o, "check offset")
		require.Equal(t, tc.expectedLength, l, "check length")
	}
}

func TestBufferedReaderAt(t *testing.T) {
	input := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}
	inputReader := bytes.NewReader(input)

	testConfigs := []struct {
		bufferSize  int
		bufferCount int
	}{
		{0, 0},
		{5, 5},
		{100, 100},
	}

	testReads := []struct {
		offset, length int64
	}{
		{0, 3},
		{0, 10},
		{5, 5},
		{9, 1},
	}

	for _, tc := range testConfigs {
		r := NewBufferedReaderAt(inputReader, int64(len(input)), tc.bufferSize, tc.bufferCount)

		for _, tr := range testReads {
			b := make([]byte, tr.length)
			r.ReadAt(b, tr.offset)
			assert.Equal(t, input[tr.offset:tr.offset+tr.length], b)
		}
	}
}