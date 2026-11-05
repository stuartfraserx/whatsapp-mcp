// Package redact masks phone numbers and JIDs before they reach logs.
package redact

import (
	"regexp"
	"strings"
)

var longDigits = regexp.MustCompile(`\+?[0-9]{6,}`)

// JID masks the user part of a JID, keeping the last two characters and the
// server: "33612345678@s.whatsapp.net" -> "***78@s.whatsapp.net".
func JID(jid string) string {
	if jid == "" {
		return ""
	}
	user, server, hasServer := strings.Cut(jid, "@")
	masked := "***"
	if r := []rune(user); len(r) > 2 {
		masked += string(r[len(r)-2:])
	}
	if !hasServer {
		return masked
	}
	return masked + "@" + server
}

// Scrub replaces every run of six or more digits, with an optional leading
// '+', by "[redacted]".
func Scrub(s string) string {
	return longDigits.ReplaceAllString(s, "[redacted]")
}
