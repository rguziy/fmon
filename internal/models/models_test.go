package models

import "testing"

func TestHashRoundTrip(t *testing.T) {
	for _, h := range []uint64{0, 1, 0x7fffffffffffffff, 0x8000000000000000, 0xef46db3751d8e999, ^uint64(0)} {
		if got := HashFromDB(HashToDB(h)); got != h {
			t.Errorf("round trip of %016x gave %016x", h, got)
		}
	}
}

func TestHashToDBReinterpretsBits(t *testing.T) {
	if got := HashToDB(^uint64(0)); got != -1 {
		t.Errorf("HashToDB(max) = %d, want -1", got)
	}
}

func TestFormatHash(t *testing.T) {
	if got := FormatHash(0xabc); got != "0000000000000abc" {
		t.Errorf("FormatHash = %q", got)
	}
}
