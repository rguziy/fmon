package hasher

import (
	"bytes"
	"context"
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/cespare/xxhash/v2"
)

func writeTemp(t *testing.T, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "f.bin")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFile(t *testing.T) {
	big := make([]byte, 3*bufferSize+12345) // spans several buffer refills
	rand.New(rand.NewSource(1)).Read(big)

	tests := []struct {
		name string
		data []byte
		want uint64
	}{
		{"empty", nil, 0xef46db3751d8e999}, // well-known XXH64 of empty input, seed 0
		{"small", []byte("hello fmon"), xxhash.Sum64([]byte("hello fmon"))},
		{"exact buffer", big[:bufferSize], xxhash.Sum64(big[:bufferSize])},
		{"large", big, xxhash.Sum64(big)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sum, n, err := File(context.Background(), writeTemp(t, tc.data))
			if err != nil {
				t.Fatal(err)
			}
			if sum != tc.want {
				t.Errorf("sum = %016x, want %016x", sum, tc.want)
			}
			if n != int64(len(tc.data)) {
				t.Errorf("n = %d, want %d", n, len(tc.data))
			}
		})
	}
}

func TestFileMissing(t *testing.T) {
	_, _, err := File(context.Background(), filepath.Join(t.TempDir(), "nope"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v, want os.ErrNotExist", err)
	}
}

func TestFileContextCancel(t *testing.T) {
	p := writeTemp(t, bytes.Repeat([]byte("x"), 2*bufferSize))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := File(ctx, p)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
