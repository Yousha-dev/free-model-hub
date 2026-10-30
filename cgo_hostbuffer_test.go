//go:build linux || darwin

package main

// This file uses OS anonymous mapping to act as a "host buffer": this memory is not on the Go heap, consistent
// with the actual C ABI passed in (Go does not support importing "C" in test files, so cgo is not used).
// The Windows side is covered by the Go memory test cases in cgo_test.go.

import (
	"bytes"
	"syscall"
	"testing"
	"unsafe"

	"free-model-hub/cpasdk/pluginabi"
)

// hostLikeBuffer allocates a block of memory matching the host buffer specifications (page-aligned, not on the Go heap).
func hostLikeBuffer(t *testing.T, payload []byte) []byte {
	t.Helper()
	size := (len(payload) + 4095) / 4096 * 4096
	region, errMap := syscall.Mmap(-1, 0, size, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_ANON|syscall.MAP_PRIVATE)
	if errMap != nil {
		t.Fatalf("mmap failed: %v", errMap)
	}
	t.Cleanup(func() {
		if errUnmap := syscall.Munmap(region); errUnmap != nil {
			t.Errorf("munmap failed: %v", errUnmap)
		}
	})
	copy(region, payload)
	return region
}

// TestBorrowedNormalizeOnHostLikeMemory runs the borrowing path on non-Go heap memory:
// verifying the result is correct and this memory is not modified.
func TestBorrowedNormalizeOnHostLikeMemory(t *testing.T) {
	payload := normalizeRequestJSON(t, "claude", 64<<10)
	want, errWant := handleMethod(pluginabi.MethodResponseNormalizeBefore, payload)
	if errWant != nil {
		t.Fatalf("copy path: %v", errWant)
	}

	region := hostLikeBuffer(t, payload)
	got := callBorrowedNormalize(t, unsafe.Pointer(unsafe.SliceData(region)), uintptr(len(payload)))
	if !bytes.Equal(got, want) {
		t.Fatal("normalization result on host memory does not match copy path")
	}
	if !bytes.Equal(region[:len(payload)], payload) {
		t.Fatal("normalization modified the host buffer")
	}
}

// TestBorrowedNormalizeOnReadOnlyHostBuffer sets the host buffer to PROT_READ before calling:
// any write during the normalization path will immediately trigger SIGSEGV, thus preserving the "read-only borrow" invariant.
func TestBorrowedNormalizeOnReadOnlyHostBuffer(t *testing.T) {
	payload := normalizeRequestJSON(t, "claude", 64<<10)
	want, errWant := handleMethod(pluginabi.MethodResponseNormalizeBefore, payload)
	if errWant != nil {
		t.Fatalf("copy path: %v", errWant)
	}

	region := hostLikeBuffer(t, payload)
	if errReadOnly := syscall.Mprotect(region, syscall.PROT_READ); errReadOnly != nil {
		t.Skipf("mprotect not available, skipping read-only page test case: %v", errReadOnly)
	}
	got := callBorrowedNormalize(t, unsafe.Pointer(unsafe.SliceData(region)), uintptr(len(payload)))
	if !bytes.Equal(got, want) {
		t.Fatal("normalization result on read-only host buffer does not match copy path")
	}
	if errWritable := syscall.Mprotect(region, syscall.PROT_READ|syscall.PROT_WRITE); errWritable != nil {
		t.Fatalf("failed to restore writability: %v", errWritable)
	}
}
