package proxy

import "testing"

func TestZeroize(t *testing.T) {
	b := []byte("sensitive prompt content")
	zeroize(b)
	for i, c := range b {
		if c != 0 {
			t.Fatalf("byte %d not zeroed: %d", i, c)
		}
	}
}

func TestZeroize_Empty(t *testing.T) {
	zeroize(nil)      // must not panic
	zeroize([]byte{}) // must not panic
}
