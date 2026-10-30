// Package hostbridge is the sole channel for communication between the plugin and the CPA host.
//
// It consolidates "host callbacks" and "outbound HTTP" because they are essentially the same thing:
// All outbound requests from the plugin must go through the host, otherwise they bypass global proxies, account-level proxies,
// outbound logs, and API statistics (manifesting as "proxy configured but does not work" or "not visible in request logs").
//
// Four things:
//
//  1. Callback channels (rpc.go / auth.go / management.go): Call / CallCtx, credential enumeration,
//     managing request contexts. CGO implementation is injected by package main (//export can only appear in the main package),
//     and test memory mock hosts replace the same injection point.
//  2. Concurrency admission: Host callbacks are non-cancellable under the current ABI; timed-out calls still occupy an OS
//     thread until the host returns, requiring rate limiting (default 64).
//  3. Shutdown draining: During Shutdown, new calls are prohibited first, then inflight calls are waited upon to return, and only then
//     is the host allowed to dlclose this dynamic library (unloading on an active Go/CGO stack will directly crash).
//  4. Outbound HTTP (http.go / sse.go): Client fetches full response at once; StreamClient
//     pulls on demand from host.http.stream_read (SSE chat); ForwardRawSSE forwards raw streams.
//
// Dependency direction toward the internal package: This package does not know the host exists; host call capabilities are injected by package main
// in init via Configure (preventing internal from reverse-importing main).

package hostbridge

// Package hostabi is the sole channel for plugin-side calls to host callbacks (host.*).
//
// It is responsible for three things:
//  1. Concurrency admission: Host callbacks are non-cancellable under the current ABI; timed-out calls still occupy an OS
//     thread until the host returns, requiring rate limiting (default 64);
//  2. Shutdown draining: During Shutdown, new calls are prohibited first, then inflight calls are waited upon to return, and only then
//     is the host allowed to dlclose this dynamic library (unloading on an active Go/CGO stack will directly crash);
//  3. Callback scope: host_callback_id is bound to the context, allowing the host to correlate upstream requests/responses
//     back to the original request and reclaim stream resources when the request ends.
//
// CGO implementation is injected by package main (//export can only appear in the main package);
// Test memory mock hosts replace the same injection point.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"free-model-hub/internal/logger"
)

// Caller is the underlying implementation of host callbacks (injected with the CGO version by main, and the memory version for tests).
type Caller func(callbackID, method string, payload any) (json.RawMessage, error)

// ErrNotConfigured indicates that the host callback implementation has not yet been injected.
var ErrNotConfigured = errors.New("hostabi: caller is not configured")

const (
	// maxConcurrentCalls is the upper limit of inflight host calls.
	maxConcurrentCalls = 64
	// shutdownDiagnosticInterval is the interval for printing diagnostic logs during the shutdown draining period.
	shutdownDiagnosticInterval = 5 * time.Second
)

var (
	callerMu sync.RWMutex
	caller   Caller

	gateMu       sync.Mutex
	shuttingDown bool
	inflightWG   sync.WaitGroup
	gate         = make(chan struct{}, maxConcurrentCalls)
)

// Configure injects the host callback implementation. Called once by main in init.
func Configure(impl Caller) {
	callerMu.Lock()
	caller = impl
	callerMu.Unlock()
}

func current() (Caller, error) {
	callerMu.RLock()
	impl := caller
	callerMu.RUnlock()
	if impl == nil {
		return nil, ErrNotConfigured
	}
	return impl, nil
}

// Call initiates a host callback.
//
// An empty callbackID indicates a background task (no request scope). Prefer using CallCtx
// to let the callback ID propagate with the context.
func Call(callbackID, method string, payload any) (json.RawMessage, error) {
	if errAcquire := acquire(); errAcquire != nil {
		return nil, errAcquire
	}
	defer release()
	impl, errCaller := current()
	if errCaller != nil {
		return nil, errCaller
	}
	return impl(callbackID, method, payload)
}

// CallCtx initiates a host callback, with the callback ID retrieved from the context.
func CallCtx(ctx context.Context, method string, payload any) (json.RawMessage, error) {
	return Call(CallbackID(ctx), method, payload)
}

// acquire requests a host call slot.
//
// Admission check and WaitGroup.Add must be completed within the same lock, otherwise Wait might observe
// a zero count, causing the shutdown drain to return early and dlclose on an active stack.
func acquire() error {
	gateMu.Lock()
	if shuttingDown {
		gateMu.Unlock()
		return errors.New("host callbacks unavailable: plugin is shutting down")
	}
	inflightWG.Add(1)
	gateMu.Unlock()
	gate <- struct{}{}
	return nil
}

func release() {
	<-gate
	inflightWG.Done()
}

// Inflight returns the current number of inflight host calls.
func Inflight() int { return len(gate) }

// WaitForShutdown closes admission and waits for all inflight host calls to return.
//
// Here **deliberately no bounded wait** is done: host callback is not cancellable, returning early would trigger
// dlclose on a still-executing Go/C stack. Outputs diagnostics every diagEvery during the wait.
func WaitForShutdown(diagEvery time.Duration) {
	gateMu.Lock()
	shuttingDown = true
	gateMu.Unlock()

	if diagEvery <= 0 {
		diagEvery = shutdownDiagnosticInterval
	}
	if Inflight() == 0 {
		inflightWG.Wait()
		return
	}
	done := make(chan struct{})
	go func() {
		inflightWG.Wait()
		close(done)
	}()
	ticker := time.NewTicker(diagEvery)
	defer ticker.Stop()
	start := time.Now()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			logger.Error("shutdown: waiting for in-flight host callbacks inflight=%d waited=%s (host callbacks are not cancelable)",
				Inflight(), time.Since(start).Round(time.Millisecond))
		}
	}
}

type callbackIDKey struct{}

// WithCallbackID binds the host callback ID to the context.
func WithCallbackID(ctx context.Context, callbackID string) context.Context {
	trimmed := strings.TrimSpace(callbackID)
	if trimmed == "" {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, callbackIDKey{}, trimmed)
}

// CallbackID retrieves the host callback ID from the context.
func CallbackID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(callbackIDKey{}).(string)
	return id
}

// MarshalPayload encodes the payload as JSON and injects host_callback_id as needed.
//
// Since the payload is any (method-specific request structs), it uses a "serialize first, then inject field" approach
// rather than adding a field to every single struct.
func MarshalPayload(callbackID string, payload any) ([]byte, error) {
	raw, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return nil, fmt.Errorf("marshal host callback payload: %w", errMarshal)
	}
	if callbackID == "" {
		return raw, nil
	}
	fields := map[string]json.RawMessage{}
	if len(raw) > 0 && string(raw) != "null" {
		if errUnmarshal := json.Unmarshal(raw, &fields); errUnmarshal != nil {
			return nil, fmt.Errorf("decode host callback payload: %w", errUnmarshal)
		}
	}
	encodedID, errID := json.Marshal(callbackID)
	if errID != nil {
		return nil, fmt.Errorf("marshal host callback id: %w", errID)
	}
	fields["host_callback_id"] = encodedID
	return json.Marshal(fields)
}

// DecodeEnvelope unpacks the envelope returned by the host callback and extracts the result part.
func DecodeEnvelope(raw []byte) (json.RawMessage, error) {
	var env struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result,omitempty"`
		Error  *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error,omitempty"`
	}
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		return nil, fmt.Errorf("decode envelope: %w", errUnmarshal)
	}
	if !env.OK {
		if env.Error != nil {
			return nil, fmt.Errorf("%s: %s", env.Error.Code, env.Error.Message)
		}
		return nil, fmt.Errorf("host callback failed")
	}
	return append(json.RawMessage(nil), env.Result...), nil
}
