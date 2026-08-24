//go:build !llamacpp

package embedded

import (
	"testing"
	"unsafe"
)

// TestProviderAtomicAlignment guards the 32-bit-alignment fix for the
// Provider's atomic fields (generation: AddUint64/LoadUint64;
// lastUsedUnixNano: StoreInt64/LoadInt64). See memory/writer.go for the class
// of bug — a misaligned 64-bit atomic panics on 386/arm. This one was
// aligned only by luck before the fix; the test makes the invariant explicit.
func TestProviderAtomicAlignment(t *testing.T) {
	var p Provider
	if off := unsafe.Offsetof(p.generation); off%8 != 0 {
		t.Errorf("Provider.generation at offset %d, must be 8-byte aligned", off)
	}
	if off := unsafe.Offsetof(p.lastUsedUnixNano); off%8 != 0 {
		t.Errorf("Provider.lastUsedUnixNano at offset %d, must be 8-byte aligned", off)
	}
}
