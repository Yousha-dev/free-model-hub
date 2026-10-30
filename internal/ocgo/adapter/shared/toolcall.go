package shared

import (
	"crypto/sha256"
	"encoding/base32"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// markedToolCallID is the upstream's own external-tool call-id shape, e.g.
// "call_00_ET_bVdDa3Ywtq34CfeXHzX21052".
var markedToolCallID = regexp.MustCompile(`^call_[0-9]+_ET_[A-Za-z0-9]+$`)

// callIndexPrefix captures the group number of an upstream-style call id.
var callIndexPrefix = regexp.MustCompile(`^call_([0-9]+)_`)

// MarkToolCallID returns id unchanged when it already carries the upstream's
// external-tool marker and otherwise derives a stable marked id from it.
//
// Background (verified against the Responses endpoint this plugin fronts): the
// upstream runs DeepSeek in thinking mode and rejects a replayed history with
// HTTP 400 "The `reasoning_text` in the thinking mode must be passed back to
// the API." whenever a function_call carries an id it does not recognize and
// the turn has no replayable reasoning item. A call whose id carries the
// upstream's own "ET" marker is accepted without any reasoning, so normalizing
// every replayed id into that shape makes reasoning-free replays valid:
// histories whose thinking was never stored, and histories whose calls were
// produced through a different route (whose ids the endpoint cannot know).
//
// The mapping is a pure function of id — index only disambiguates id-less
// calls — so a function_call and its function_call_output always normalize to
// the same value and repeated replays stay stable (idempotent: already-marked
// ids pass through untouched).
func MarkToolCallID(id string, index int) string {
	call := strings.TrimSpace(id)
	if markedToolCallID.MatchString(call) {
		return call
	}
	seed := call
	if seed == "" {
		// id-less calls: keep siblings distinct by folding their position in.
		seed = fmt.Sprintf("#%d", index)
	}
	sum := sha256.Sum256([]byte(seed))
	suffix := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:])
	group := ""
	if m := callIndexPrefix.FindStringSubmatch(call); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil {
			group = fmt.Sprintf("%02d", n%100)
		}
	}
	if group == "" {
		group = fmt.Sprintf("%02d", int(sum[0])%100)
	}
	return "call_" + group + "_ET_" + suffix[:20]
}
