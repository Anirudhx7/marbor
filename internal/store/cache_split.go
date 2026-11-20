package store

import (
	"log"
	"strconv"
	"sync/atomic"
)

// MaxCacheSplitTokens caps one request's prompt or cached token count. A value
// above it is not believable for a single request and is treated as not
// reported rather than clamped, so a hostile or buggy upstream cannot skew the
// window sums.
const MaxCacheSplitTokens = 1 << 40

// ValidCacheSplit reports whether input and cached form a usable prompt-cache
// split: both present, input > 0 and within MaxCacheSplitTokens, and
// 0 <= cached <= input. It is the single definition every writer and reader of
// the split uses.
func ValidCacheSplit(input, cached *int64) bool {
	return input != nil && cached != nil &&
		*input > 0 && *input <= MaxCacheSplitTokens &&
		*cached >= 0 && *cached <= *input
}

// rejectedSplits counts non-nil splits dropped as invalid, so the log notes the
// first one and then only every rejectedSplitLogEvery-th, never one per request.
var rejectedSplits atomic.Int64

const rejectedSplitLogEvery = 1000

// NoteRejectedCacheSplit records that a reported split was dropped as invalid.
// where names the caller. It logs the first drop and then one in every
// rejectedSplitLogEvery.
func NoteRejectedCacheSplit(where string, input, cached *int64) {
	n := rejectedSplits.Add(1)
	if n != 1 && n%rejectedSplitLogEvery != 0 {
		return
	}
	log.Printf("store: %s dropped an invalid cache split (input=%s cached=%s); %d dropped so far, further drops are logged every %d",
		where, fmtSplitVal(input), fmtSplitVal(cached), n, rejectedSplitLogEvery)
}

func fmtSplitVal(p *int64) string {
	if p == nil {
		return "unset"
	}
	return strconv.FormatInt(*p, 10)
}

// SanitizeCacheSplit returns copies of the pair when it is a valid split and
// nil, nil otherwise, so a malformed pair can never be recorded and callers
// never share pointers. A dropped non-nil pair is logged through
// NoteRejectedCacheSplit under the caller's name.
func SanitizeCacheSplit(where string, input, cached *int64) (in, ca *int64) {
	if input == nil && cached == nil {
		return nil, nil
	}
	if !ValidCacheSplit(input, cached) {
		NoteRejectedCacheSplit(where, input, cached)
		return nil, nil
	}
	i, c := *input, *cached
	return &i, &c
}

// readCacheSplit validates a stored pair on read: an invalid or half-set pair
// reads as not reported, without logging per row.
func readCacheSplit(input, cached *int64) (in, ca *int64) {
	if !ValidCacheSplit(input, cached) {
		return nil, nil
	}
	return input, cached
}
