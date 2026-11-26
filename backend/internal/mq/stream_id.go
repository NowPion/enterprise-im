package mq

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var secretPattern = regexp.MustCompile(`(?i)(redis://[^\s]+|mysql://[^\s]+|password\s*=\s*\S+|pwd\s*=\s*\S+)`)

func parseStreamID(id string) (uint64, uint64, bool) {
	trimmed := strings.TrimSpace(id)
	if trimmed == "" {
		return 0, 0, false
	}
	msPart, seqPart, hasSeq := strings.Cut(trimmed, "-")
	ms, err := strconv.ParseUint(msPart, 10, 64)
	if err != nil {
		return 0, 0, false
	}
	if !hasSeq {
		return ms, 0, true
	}
	seq, err := strconv.ParseUint(seqPart, 10, 64)
	if err != nil {
		return 0, 0, false
	}
	return ms, seq, true
}

func compareStreamID(a, b string) int {
	msA, seqA, okA := parseStreamID(a)
	msB, seqB, okB := parseStreamID(b)
	if !okA && !okB {
		return 0
	}
	if !okA {
		return -1
	}
	if !okB {
		return 1
	}
	if msA < msB {
		return -1
	}
	if msA > msB {
		return 1
	}
	if seqA < seqB {
		return -1
	}
	if seqA > seqB {
		return 1
	}
	return 0
}

func minStreamID(a, b string) string {
	if compareStreamID(a, b) <= 0 {
		return a
	}
	return b
}

func nextStreamID(id string) string {
	ms, seq, ok := parseStreamID(id)
	if !ok {
		return "0-0"
	}
	if seq == ^uint64(0) {
		return fmt.Sprintf("%d-0", ms+1)
	}
	return fmt.Sprintf("%d-%d", ms, seq+1)
}

func streamIDTime(id string) (time.Time, bool) {
	ms, _, ok := parseStreamID(id)
	if !ok || ms == 0 {
		return time.Time{}, false
	}
	return time.UnixMilli(int64(ms)).UTC(), true
}

func stringField(values map[string]any, key string) string {
	if len(values) == 0 {
		return ""
	}
	raw, ok := values[key]
	if !ok || raw == nil {
		return ""
	}
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v)
	case []byte:
		return strings.TrimSpace(string(v))
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}

func parseUint64Field(values map[string]any, key string) (uint64, bool) {
	raw := stringField(values, key)
	if raw == "" {
		return 0, false
	}
	val, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, false
	}
	return val, true
}

func formatUint64CSV(ids []uint64) string {
	deduped := dedupePositiveUint64(ids)
	if len(deduped) == 0 {
		return ""
	}
	parts := make([]string, 0, len(deduped))
	for _, id := range deduped {
		parts = append(parts, strconv.FormatUint(id, 10))
	}
	return strings.Join(parts, ",")
}

func dedupePositiveUint64(ids []uint64) []uint64 {
	if len(ids) == 0 {
		return nil
	}
	seen := make(map[uint64]struct{}, len(ids))
	out := make([]uint64, 0, len(ids))
	for _, id := range ids {
		if id == 0 {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

func parseTargetUserIDs(values map[string]any) []uint64 {
	raw := stringField(values, "target_user_ids")
	var parsed []uint64
	if raw != "" {
		if strings.HasPrefix(raw, "[") {
			var strIDs []string
			if json.Unmarshal([]byte(raw), &strIDs) == nil {
				for _, s := range strIDs {
					if id, err := strconv.ParseUint(strings.TrimSpace(s), 10, 64); err == nil && id > 0 {
						parsed = append(parsed, id)
					}
				}
			} else {
				var numIDs []uint64
				if json.Unmarshal([]byte(raw), &numIDs) == nil {
					parsed = append(parsed, numIDs...)
				}
			}
		} else {
			for _, part := range strings.Split(raw, ",") {
				if id, err := strconv.ParseUint(strings.TrimSpace(part), 10, 64); err == nil && id > 0 {
					parsed = append(parsed, id)
				}
			}
		}
	}
	if single, ok := parseUint64Field(values, "target_user_id"); ok && single > 0 {
		parsed = append(parsed, single)
	}
	return dedupePositiveUint64(parsed)
}

func sanitizeErrorSummary(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.TrimSpace(err.Error())
	msg = secretPattern.ReplaceAllString(msg, "[REDACTED]")
	runes := []rune(msg)
	if len(runes) > 512 {
		return string(runes[:512])
	}
	return msg
}
