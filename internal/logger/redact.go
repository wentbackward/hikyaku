package logger

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
)

// Log-safe renderings for levels 0-2, which must not carry caller or upstream
// text (see the package doc).
//
// Errors are rendered by allowlist, from their structure, never by editing
// their text: the standard library puts peer bytes into error strings in more
// ways than any filter can know (Go's HTTP transport quotes a non-HTTP peer's
// reply, net/textproto copies a malformed header line unquoted, encoding/json
// single-quotes the offending character, the YAML decoder backquotes a scalar
// without escaping it). SafeErr prints only fields of known error types that
// cannot carry such bytes; anything else is its type and length. The full
// text belongs on the body path (level 3, compiled out in hardened builds):
// see BodyErr.

// SafeError is implemented by errors that know a log-safe rendering of
// themselves: one built only from fixed text, numbers, and values that came
// from neither a caller nor an upstream. Core's own error types implement it.
type SafeError interface {
	SafeError() string
}

// SafeErr renders err for a log line at levels 0-2. It walks err's chain and
// renders the first error of a known type from its safe fields only:
//
//   - a SafeError: its SafeError();
//   - *url.Error: the operation, the URL as SafeURL renders it, and its cause;
//   - *net.OpError: the operation, network, addresses and cause;
//   - *os.SyscallError: the system call and cause;
//   - *net.DNSError: the name looked up and whether it was not found or
//     timed out;
//   - x509 and TLS errors: the error's kind, as fixed text;
//   - *fs.PathError: the operation, path and cause;
//   - *http.MaxBytesError: the limit;
//   - syscall.Errno and the standard sentinels (io.EOF, context.Canceled,
//     os.ErrDeadlineExceeded, ...): their fixed text.
//
// An error with no known type in its chain renders as
// <error type=T1,T2,... len=N> (the chain's types and the text's length,
// plus "timeout" when it reports one and reason=<phrase> when a chain
// link's own text opens with one of knownReasons), never its text. N is the
// length of the full text, so it reflects the size of whatever the error
// carries.
func SafeErr(err error) string {
	if err == nil {
		return "<nil>"
	}
	if s, ok := renderChain(err); ok {
		return s
	}
	var types []string
	for e := err; e != nil; e = errors.Unwrap(e) {
		types = append(types, fmt.Sprintf("%T", e))
	}
	extra := ""
	var te interface{ Timeout() bool }
	if errors.As(err, &te) && te.Timeout() {
		extra = " timeout"
	}
	text := err.Error()
	if r := knownReason(err); r != "" {
		extra += " reason=" + strings.ReplaceAll(r, " ", "_")
	}
	return fmt.Sprintf("<error type=%s len=%d%s>", strings.Join(types, ","), len(text), extra)
}

// knownReasons are fixed phrases that open the standard library's untyped
// errors. The rendering names a phrase only when a chain link's own text
// (its text without its cause's, after a known package prefix) starts with
// it, and prints this constant, never the error's text. A phrase further
// into the text, where the library pastes a peer's bytes, is ignored, so a
// peer cannot choose the reason. Longer phrases come first so the most
// specific one wins.
var knownReasons = []string{
	"malformed MIME header initial line",
	"malformed MIME header line",
	"malformed HTTP status code",
	"malformed HTTP response",
	"malformed HTTP version",
	"server gave HTTP response to HTTPS client",
	"timeout awaiting response headers",
	"TLS handshake timeout",
	"HTTP/1.x transport connection broken",
	"invalid header field",
	"too many transfer encodings",
	"unsupported transfer encoding",
	"bad Content-Length",
	"invalid byte in chunk length",
}

// knownPrefixes are package prefixes the standard library puts before its
// error phrases.
var knownPrefixes = []string{"net/http: ", "http: ", "net/textproto: "}

// knownReason returns the phrase that opens the own text of the innermost
// chain link that has one, or "".
func knownReason(err error) string {
	var links []error
	for e := err; e != nil; e = errors.Unwrap(e) {
		links = append(links, e)
	}
	for i := len(links) - 1; i >= 0; i-- {
		own := links[i].Error()
		if i+1 < len(links) {
			own = strings.TrimSuffix(own, links[i+1].Error())
		}
		for _, p := range knownPrefixes {
			own = strings.TrimPrefix(own, p)
		}
		for _, r := range knownReasons {
			if strings.HasPrefix(own, r) {
				return r
			}
		}
	}
	return ""
}

// BodyErr logs err's full text on the body path (level 3; a no-op in
// hardened builds) when it differs from what SafeErr shows, so inspect
// builds keep the detail for debugging. prefix names the log line.
func BodyErr(prefix string, err error) {
	if err == nil || Get() < LevelBody {
		return
	}
	if full := err.Error(); full != SafeErr(err) {
		Body("%s: %s", prefix, full)
	}
}

// renderChain renders the first known error in err's chain, depth first.
func renderChain(err error) (string, bool) {
	if s, ok := renderKnown(err); ok {
		return s, true
	}
	switch u := err.(type) { //nolint:errorlint // walks the chain one link at a time
	case interface{ Unwrap() error }:
		if in := u.Unwrap(); in != nil {
			return renderChain(in)
		}
	case interface{ Unwrap() []error }:
		for _, in := range u.Unwrap() {
			if in == nil {
				continue
			}
			if s, ok := renderChain(in); ok {
				return s, true
			}
		}
	}
	return "", false
}

// sentinels are errors whose text is fixed; they are compared by identity.
var sentinels = []error{
	io.EOF, io.ErrUnexpectedEOF, io.ErrClosedPipe, io.ErrShortWrite,
	context.Canceled, context.DeadlineExceeded,
	os.ErrDeadlineExceeded, os.ErrNotExist, os.ErrExist, os.ErrPermission, os.ErrClosed,
	net.ErrClosed,
	http.ErrAbortHandler, http.ErrBodyReadAfterClose, http.ErrHandlerTimeout,
	http.ErrServerClosed, http.ErrUseLastResponse, http.ErrSchemeMismatch,
}

func renderKnown(err error) (string, bool) {
	for _, s := range sentinels {
		if err == s { //nolint:errorlint // identity of this chain link; renderChain walks the chain
			return s.Error(), true
		}
	}
	switch e := err.(type) { //nolint:errorlint // one chain link; renderChain does the walking
	case SafeError:
		return e.SafeError(), true
	case *url.Error:
		return fmt.Sprintf("%s %s: %s", e.Op, SafeURLString(e.URL), SafeErr(e.Err)), true
	case *net.OpError:
		var b strings.Builder
		b.WriteString(e.Op)
		if e.Net != "" {
			b.WriteString(" " + e.Net)
		}
		if e.Source != nil {
			b.WriteString(" " + e.Source.String())
			if e.Addr != nil {
				b.WriteString("->")
			}
		}
		if e.Addr != nil {
			if e.Source == nil {
				b.WriteString(" ")
			}
			b.WriteString(e.Addr.String())
		}
		b.WriteString(": " + SafeErr(e.Err))
		return b.String(), true
	case *os.SyscallError:
		return e.Syscall + ": " + SafeErr(e.Err), true
	case *net.DNSError:
		switch {
		case e.IsNotFound:
			return "lookup " + e.Name + ": no such host", true
		case e.IsTimeout:
			return "lookup " + e.Name + ": timeout", true
		}
		return "lookup " + e.Name + ": dns error", true
	case *fs.PathError:
		return e.Op + " " + e.Path + ": " + SafeErr(e.Err), true
	case *http.MaxBytesError:
		return fmt.Sprintf("http: request body too large (limit %d bytes)", e.Limit), true
	case syscall.Errno:
		return e.Error(), true
	case *tls.CertificateVerificationError:
		return "tls: failed to verify certificate: " + SafeErr(e.Err), true
	case tls.RecordHeaderError:
		return "tls: first record does not look like a TLS handshake", true
	case tls.AlertError:
		return e.Error(), true // a fixed alert name
	case x509.UnknownAuthorityError:
		return "x509: certificate signed by unknown authority", true
	case x509.HostnameError:
		return "x509: certificate is not valid for the requested host", true
	case x509.CertificateInvalidError:
		return fmt.Sprintf("x509: certificate is invalid (reason %d)", e.Reason), true
	case x509.SystemRootsError:
		return "x509: failed to load system roots", true
	case x509.ConstraintViolationError:
		return "x509: certificate violates a constraint", true
	case x509.UnhandledCriticalExtension:
		return "x509: unhandled critical extension", true
	case x509.InsecureAlgorithmError:
		return e.Error(), true // a fixed algorithm name
	}
	return "", false
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

// SafeToken returns s when it is 1 to maxLen token characters (letters,
// digits and -_.:/@+), such as an id or a model name, and <len=N> otherwise:
// a peer-supplied identifier is logged only when it cannot carry text.
func SafeToken(s string, maxLen int) string {
	if s == "" || len(s) > maxLen {
		return fmt.Sprintf("<len=%d>", len(s))
	}
	for _, r := range s {
		if !tokenRune(r) {
			return fmt.Sprintf("<len=%d>", len(s))
		}
	}
	return s
}

func tokenRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_.:/@+", r)
}
