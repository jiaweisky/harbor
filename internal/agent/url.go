package agent

import "strings"

// ListenURL extracts the first http(s) URL from a line of CLI output, if any. It
// is used to read the address a spawned server reports on stdout (the line may
// carry ANSI color codes and trailing text).
func ListenURL(line string) (string, bool) {
	i := strings.Index(line, "http://")
	if i < 0 {
		i = strings.Index(line, "https://")
	}
	if i < 0 {
		return "", false
	}
	rest := line[i:]
	if j := strings.IndexAny(rest, " \t\r\n"); j >= 0 {
		rest = rest[:j]
	}
	return strings.TrimRight(rest, "/"), true
}
