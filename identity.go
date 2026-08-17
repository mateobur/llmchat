package main

import (
	"fmt"
	"math"
	"regexp"
	"strings"
)

// Palette is the set of suggested colors offered to joining participants.
// Every pair is far enough apart to stay tellable apart on a dark background
// (enforced by TestPaletteIsDistinguishable).
var Palette = []string{
	"#e6194b", // crimson
	"#3cb44b", // green
	"#4363d8", // blue
	"#f58231", // orange
	"#911eb4", // purple
	"#42d4f4", // cyan
	"#f032e6", // magenta
	"#bfef45", // lime
	"#fabed4", // pink
	"#469990", // teal
	"#dcbeff", // lavender
	"#9a6324", // brown
	"#fffac8", // beige
	"#aaffc3", // mint
	"#ffd8b1", // apricot
	"#ffe119", // yellow
}

const (
	maxHandleLen = 24
	minHandleLen = 2
)

var handleRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// reservedHandles cannot be claimed: the server itself posts as "system", and
// the rest would make @-mentions ambiguous.
var reservedHandles = map[string]bool{
	"system": true, "server": true, "all": true, "everyone": true,
	"here": true, "channel": true, "me": true, "you": true,
}

// NormalizeHandle validates a requested handle and returns it unchanged on
// success. Handles are compared case-insensitively but displayed as typed.
func NormalizeHandle(raw string) (string, error) {
	h := strings.TrimSpace(raw)
	if h == "" {
		return "", fmt.Errorf("handle is required")
	}
	if len([]rune(h)) < minHandleLen {
		return "", fmt.Errorf("handle must be at least %d characters", minHandleLen)
	}
	if len([]rune(h)) > maxHandleLen {
		return "", fmt.Errorf("handle must be at most %d characters", maxHandleLen)
	}
	if !handleRE.MatchString(h) {
		return "", fmt.Errorf("handle may only contain letters, digits, dot, dash and underscore, and must start with a letter or digit")
	}
	if reservedHandles[strings.ToLower(h)] {
		return "", fmt.Errorf("handle %q is reserved", h)
	}
	return h, nil
}

// HandleKey is the case-insensitive uniqueness key for a handle.
func HandleKey(handle string) string { return strings.ToLower(handle) }

var hex3RE = regexp.MustCompile(`^#?([0-9a-fA-F]{3})$`)
var hex6RE = regexp.MustCompile(`^#?([0-9a-fA-F]{6})$`)

// NormalizeColor accepts "#rgb", "#rrggbb" (with or without the leading "#")
// and returns the canonical lowercase "#rrggbb" form used as uniqueness key.
func NormalizeColor(raw string) (string, error) {
	c := strings.TrimSpace(raw)
	if c == "" {
		return "", fmt.Errorf("color is required")
	}
	if m := hex6RE.FindStringSubmatch(c); m != nil {
		return "#" + strings.ToLower(m[1]), nil
	}
	if m := hex3RE.FindStringSubmatch(c); m != nil {
		s := strings.ToLower(m[1])
		return fmt.Sprintf("#%c%c%c%c%c%c", s[0], s[0], s[1], s[1], s[2], s[2]), nil
	}
	return "", fmt.Errorf("color %q is not a hex color like #4363d8", raw)
}

func rgb(color string) (r, g, b float64) {
	var ri, gi, bi int
	fmt.Sscanf(color, "#%02x%02x%02x", &ri, &gi, &bi)
	return float64(ri), float64(gi), float64(bi)
}

// ColorDistance is the "redmean" weighted RGB distance: a cheap approximation
// of perceptual difference, ranging from 0 (identical) to ~765 (black/white).
func ColorDistance(a, b string) float64 {
	r1, g1, b1 := rgb(a)
	r2, g2, b2 := rgb(b)
	rmean := (r1 + r2) / 2
	dr, dg, db := r1-r2, g1-g2, b1-b2
	return math.Sqrt((2+rmean/256)*dr*dr + 4*dg*dg + (2+(255-rmean)/256)*db*db)
}
