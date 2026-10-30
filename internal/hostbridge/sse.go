package hostbridge

// This file provides pass-through forwarding when "upstream SSE is isomorphic to the host contract".
//
// Why placed in httpx: it only depends on io, acting as a general outbound/inbound utility independent of any provider.
// Provider-specific SSE rewriting (such as WorkBuddy whitelist reconstruction, Qoder protocol translation) remains
// in their respective packages.

import (
	"bufio"
	"io"
	"strings"
)

// SSEScannerBuffer is the upper limit for a single frame (the default 64KB truncates long frames like tool arguments).
const SSEScannerBuffer = 8 * 1024 * 1024

// ForwardRawSSE forwards upstream SSE payloads frame by frame.
//
// Behavioral convention (aligned with the host contract):
//   - The `data: ` prefix is added by the host; only the payload is passed here;
//   - `[DONE]` is the stream termination signal and is not forwarded (the host will append its own);
//   - Non-data lines (comments, event:) are forwarded as-is to preserve the upstream frame structure;
//   - Empty payloads are skipped.
//
// Having zero valid frames throughout is not an error: upstream sometimes returns a stream without any delta,
// and treating it as an error causes the client to receive a meaningless error frame.
func ForwardRawSSE(body io.Reader, emit func([]byte) error) error {
	if body == nil {
		return nil
	}
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), SSEScannerBuffer)

	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if !strings.HasPrefix(trimmed, "data:") {
			// Comment and event: lines are sent down as-is to preserve the upstream frame structure.
			if errEmit := emit([]byte(line)); errEmit != nil {
				return errEmit
			}
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		if payload == "[DONE]" {
			return nil
		}
		if payload == "" {
			continue
		}
		if errEmit := emit([]byte(payload)); errEmit != nil {
			return errEmit
		}
	}
	return scanner.Err()
}
