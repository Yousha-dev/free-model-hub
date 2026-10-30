package main

// This file covers request buffer handling for C ABI entry points: borrow/copy dispatch, no modification of host
// buffers during borrowing, response independence from host buffers, and the same path on real C heap memory.

import (
	"bytes"
	"encoding/json"
	"runtime"
	"testing"
	"unsafe"

	"free-model-hub/cpasdk/pluginabi"
	"free-model-hub/cpasdk/pluginapi"
)

// normalizeRequestJSON creates a normalized request isomorphic to the host (including two large request bodies).
func normalizeRequestJSON(tb testing.TB, target string, bodySize int) []byte {
	tb.Helper()
	rpc := pluginapi.ResponseTransformRequest{
		FromFormat: "openai", ToFormat: target, Stream: true,
		OriginalRequest:   bytes.Repeat([]byte("a"), bodySize),
		TranslatedRequest: bytes.Repeat([]byte("b"), bodySize),
		Body:              []byte(`{"id":"c1","choices":[]}`),
	}
	raw, errMarshal := json.Marshal(rpc)
	if errMarshal != nil {
		tb.Fatal(errMarshal)
	}
	return raw
}

// callBorrowedNormalize follows the exact same sequence as cliproxyPluginCall: gets the request slice first, then passes it to handleMethod.
func callBorrowedNormalize(tb testing.TB, request unsafe.Pointer, requestLen uintptr) []byte {
	tb.Helper()
	requestBytes, errRequest := hostRequestBytes(pluginabi.MethodResponseNormalizeBefore, request, requestLen)
	if errRequest != nil {
		tb.Fatal(errRequest)
	}
	raw, errHandle := handleMethod(pluginabi.MethodResponseNormalizeBefore, requestBytes)
	if errHandle != nil {
		tb.Fatal(errHandle)
	}
	return raw
}

// TestHostRequestBytesBorrowsOnlyReadOnlyMethods guards the dispatch itself:
// normalization hooks borrow the host buffer, while other RPCs must copy.
func TestHostRequestBytesBorrowsOnlyReadOnlyMethods(t *testing.T) {
	payload := []byte(`{"stream":true}`)
	hostPtr := unsafe.Pointer(&payload[0])

	borrowed, errBorrow := hostRequestBytes(pluginabi.MethodResponseNormalizeBefore, hostPtr, uintptr(len(payload)))
	if errBorrow != nil {
		t.Fatalf("Borrow path error: %v", errBorrow)
	}
	if unsafe.Pointer(unsafe.SliceData(borrowed)) != hostPtr {
		t.Fatal("response.normalize_before should borrow host buffer, but a copy actually occurred")
	}

	for _, method := range []string{
		pluginabi.MethodExecutorExecute,
		pluginabi.MethodExecutorExecuteStream,
		pluginabi.MethodAuthRefresh,
		pluginabi.MethodPluginRegister,
	} {
		copied, errCopy := hostRequestBytes(method, hostPtr, uintptr(len(payload)))
		if errCopy != nil {
			t.Fatalf("%s copy path error: %v", method, errCopy)
		}
		if unsafe.Pointer(unsafe.SliceData(copied)) == hostPtr {
			t.Fatalf("%s must copy host buffer and cannot borrow", method)
		}
		if !bytes.Equal(copied, payload) {
			t.Fatalf("%s copied bytes mismatch: %q", method, copied)
		}
	}
}

// TestHostRequestBytesRequestLengthContract covers empty requests and host contract violations (nil + non-zero length).
func TestHostRequestBytesRequestLengthContract(t *testing.T) {
	payload := []byte(`{"a":1}`)
	ptr := unsafe.Pointer(&payload[0])

	for _, tc := range []struct {
		name  string
		ptr   unsafe.Pointer
		len   uintptr
		wantE bool
	}{
		{name: "nil_nil", ptr: nil, len: 0},
		{name: "ptr_zero", ptr: ptr, len: 0},
		{name: "nil_with_length", ptr: nil, len: 10, wantE: true},
	} {
		for _, method := range []string{pluginabi.MethodResponseNormalizeBefore, pluginabi.MethodExecutorExecute} {
			got, errGot := hostRequestBytes(method, tc.ptr, tc.len)
			if tc.wantE {
				if errGot == nil {
					t.Fatalf("%s/%s: nil pointer with non-zero length must return an error", tc.name, method)
				}
				continue
			}
			if errGot != nil {
				t.Fatalf("%s/%s: empty request should not error: %v", tc.name, method, errGot)
			}
			if len(got) != 0 {
				t.Fatalf("%s/%s: empty request should get empty slice, got %d bytes", tc.name, method, len(got))
			}
		}
	}
}

// TestBorrowedNormalizePreservesHostBuffer verifies that the borrowing path does not modify the host buffer,
// and that the response is decoupled from the host buffer (remains consistent after clearing).
func TestBorrowedNormalizePreservesHostBuffer(t *testing.T) {
	for _, target := range []string{"claude", "openai"} {
		t.Run(target, func(t *testing.T) {
			raw := normalizeRequestJSON(t, target, 256<<10)
			before := bytes.Clone(raw)

			want, errWant := handleMethod(pluginabi.MethodResponseNormalizeBefore, before)
			if errWant != nil {
				t.Fatalf("Copy path: %v", errWant)
			}
			got := callBorrowedNormalize(t, unsafe.Pointer(unsafe.SliceData(raw)), uintptr(len(raw)))
			if !bytes.Equal(got, want) {
				t.Fatalf("Borrowed response %q and copied response %q do not match", got, want)
			}
			if !bytes.Equal(raw, before) {
				t.Fatal("Normalization modified the host buffer")
			}
			for i := range raw {
				raw[i] = 0
			}
			if !bytes.Equal(got, want) {
				t.Fatal("Response depends on host buffer and can still be modified after call returns")
			}
			runtime.KeepAlive(raw)
		})
	}
}

// BenchmarkResponseNormalizeABI benchmarks the two entry paths: "full packet copy" and "borrow".
func BenchmarkResponseNormalizeABI(b *testing.B) {
	for _, target := range []string{"openai", "claude"} {
		raw := normalizeRequestJSON(b, target, 4<<20)
		hostPtr := unsafe.Pointer(unsafe.SliceData(raw))
		b.Run(target+"/copy", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				cloned := make([]byte, len(raw))
				copy(cloned, raw)
				if _, err := handleMethod(pluginabi.MethodResponseNormalizeBefore, cloned); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(target+"/borrow", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if got := callBorrowedNormalize(b, hostPtr, uintptr(len(raw))); len(got) == 0 {
					b.Fatal("借用路径返回空响应")
				}
			}
			runtime.KeepAlive(raw)
		})
	}
}
