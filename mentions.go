package main

import (
	"regexp"
	"strings"
)

// mentionRE finds @handle tokens. The preceding character is checked separately
// so that overlapping candidates ("@ada @bob") and e-mail addresses
// ("someone@example.com") are handled correctly.
var mentionRE = regexp.MustCompile(`@([a-zA-Z0-9][a-zA-Z0-9._-]*)`)

// broadcastMentions address the whole room. They are also reserved handles, so
// there is never a real participant they could be confused with.
var broadcastMentions = map[string]bool{
	"all": true, "everyone": true, "here": true, "channel": true,
}

// ParseMentions extracts the handles mentioned in a message, normalized to
// lowercase and deduplicated, in the order they appear. A mention is recorded
// whether or not that handle is currently in the room: someone tagged while
// away should still find it later.
func ParseMentions(text string) []string {
	var out []string
	seen := map[string]bool{}

	for _, m := range mentionRE.FindAllStringSubmatchIndex(text, -1) {
		if start := m[0]; start > 0 && !mentionBoundary(text[start-1]) {
			continue // part of a longer word or an e-mail address
		}
		// Trailing punctuation is far more likely to be prose than part of a
		// handle: "ask @ada." mentions ada.
		token := strings.TrimRight(text[m[2]:m[3]], "._-")
		if len(token) < minHandleLen || len(token) > maxHandleLen {
			continue
		}
		key := HandleKey(token)
		if !seen[key] {
			seen[key] = true
			out = append(out, key)
		}
	}
	return out
}

// mentionBoundary reports whether a byte can precede a mention.
func mentionBoundary(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return false
	case b == '_', b == '@', b == '.', b == '-':
		return false
	}
	return true
}

// MentionsHandle reports whether a parsed mention list addresses the given
// handle, either by name or through a broadcast keyword like @everyone.
func MentionsHandle(mentions []string, handle string, includeBroadcast bool) bool {
	key := HandleKey(handle)
	for _, m := range mentions {
		if m == key {
			return true
		}
		if includeBroadcast && broadcastMentions[m] {
			return true
		}
	}
	return false
}
