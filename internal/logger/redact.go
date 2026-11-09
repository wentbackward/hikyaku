package logger

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Helpers for levels 0-2, which must not carry caller or upstream text (see
// the package doc). Errors are the hard case: Go's HTTP transport quotes the
// bytes a non-HTTP peer sent back ("malformed HTTP response %q"), a
// *url.Error quotes the request URL with its query string, and the YAML
// decoder quotes the offending scalar ("cannot unmarshal !!str `...`").

// RedactQuoted replaces every double-quoted (Go syntax) or backquoted span
// of s with <quoted len=N>, N being the length of the quoted text. An
// unterminated quote redacts the rest of s.
func RedactQuoted(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		switch s[i] {
		case '"':
			if q, err := strconv.QuotedPrefix(s[i:]); err == nil {
				u, _ := strconv.Unquote(q) // QuotedPrefix guarantees it unquotes
				fmt.Fprintf(&b, "<quoted len=%d>", len(u))
				i += len(q)
				continue
			}
			fmt.Fprintf(&b, "<quoted len=%d>", len(s)-i-1)
			return b.String()
		case '`':
			end := strings.IndexByte(s[i+1:], '`')
			if end < 0 {
				fmt.Fprintf(&b, "<quoted len=%d>", len(s)-i-1)
				return b.String()
			}
			fmt.Fprintf(&b, "<quoted len=%d>", end)
			i += end + 2
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// SafeURL renders u with its scheme, host and path only: no user info (a
// credential), query string (caller text the proxy forwards) or fragment.
func SafeURL(u *url.URL) string {
	if u == nil {
		return ""
	}
	c := url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path}
	return c.String()
}

// SafeURLString is SafeURL for a URL in string form; one that does not parse
// is rendered as its length.
func SafeURLString(s string) string {
	u, err := url.Parse(s)
	if err != nil {
		return fmt.Sprintf("<unparsable url len=%d>", len(s))
	}
	return SafeURL(u)
}

// SafeErr renders err for levels 0-2. A *url.Error keeps its operation and
// its URL through SafeURL; every quoted span of the rest is redacted
// (RedactQuoted), whatever produced it.
func SafeErr(err error) string {
	if err == nil {
		return "<nil>"
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		inner := "<nil>"
		if ue.Err != nil {
			inner = RedactQuoted(ue.Err.Error())
		}
		return fmt.Sprintf("%s %s: %s", ue.Op, SafeURLString(ue.URL), inner)
	}
	return RedactQuoted(err.Error())
}
