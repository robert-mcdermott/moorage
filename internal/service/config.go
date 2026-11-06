package service

import (
	"html"
	"regexp"
	"strings"
)

var plistConfig = regexp.MustCompile(`<string>--config</string>\s*<string>([^<]*)</string>`)

// ConfigIn says which configuration a service's definition runs: what follows --config, as each service manager has it written. It is empty if none is named. Going by
// this rather than looking for a path in the text, a path with something in it that has to be escaped is still found, and one that is only a part of another is not.
func ConfigIn(definition []byte) string {
	text := string(definition)
	if strings.Contains(text, "<plist") {
		m := plistConfig.FindStringSubmatch(text)
		if m == nil {
			return ""
		}
		return html.UnescapeString(m[1])
	}
	_, rest, found := strings.Cut(text, "--config ")
	if !found {
		return ""
	}
	rest = strings.TrimLeft(rest, " \t")
	if strings.Contains(text, "ExecStart=") {
		return strings.ReplaceAll(systemdWord(rest), "%%", "%")
	}
	return windowsWord(rest)
}

// systemdWord reads the first word of what systemdArgs wrote, where a backslash in quotes always says that what follows is meant as it is.
func systemdWord(s string) string {
	if !strings.HasPrefix(s, `"`) {
		return firstField(s)
	}
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
			if i < len(s) {
				b.WriteByte(s[i])
			}
		case '"':
			return b.String()
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// windowsWord reads the first word of a Windows command line, where backslashes only mean anything in front of a quote, or they would not do for a path.
func windowsWord(s string) string {
	if !strings.HasPrefix(s, `"`) {
		return firstField(s)
	}
	var b strings.Builder
	slashes := 0
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			slashes++
		case '"':
			b.WriteString(strings.Repeat(`\`, slashes/2))
			if slashes%2 == 0 {
				return b.String()
			}
			b.WriteByte('"')
			slashes = 0
		default:
			b.WriteString(strings.Repeat(`\`, slashes))
			slashes = 0
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

func firstField(s string) string {
	if fields := strings.Fields(s); len(fields) > 0 {
		return fields[0]
	}
	return ""
}
