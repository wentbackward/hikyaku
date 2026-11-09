package proxy

import (
	"bufio"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Response encoding on the upstream leg.
//
// The proxy reads every response body it forwards (usage metering, stream
// loop detection, capture, content logging), so it owns the Accept-Encoding
// negotiation with the upstream: the director always asks for identity
// (upstreamAcceptEncoding), whatever the caller or a headers rule asked for.
// Asking for identity rather than gzip keeps every transport (core's own or an
// embedder's TransportFactory) on the same footing, keeps streamed events
// flowing as the upstream writes them, and keeps decompressor state (which
// holds recent plaintext the proxy cannot wipe) out of the request path.
//
// An upstream that answers encoded anyway is handled by decodeUpstreamBody:
// gzip and deflate are decoded before anything reads the body; any other
// coding is refused, because forwarding bytes nothing could meter would turn
// a misbehaving upstream into free traffic.

// upstreamAcceptEncoding is the Accept-Encoding every proxied request carries.
const upstreamAcceptEncoding = "identity"

// maxCodingInError bounds how much of an upstream-supplied Content-Encoding
// value is quoted back in an error.
const maxCodingInError = 64

// errUnreadableEncoding reports a response in a content coding the proxy
// cannot decode.
type errUnreadableEncoding struct{ coding string }

func (e errUnreadableEncoding) Error() string {
	c := e.coding
	if len(c) > maxCodingInError {
		c = c[:maxCodingInError] + "..."
	}
	return fmt.Sprintf("upstream answered in content encoding %q, which the proxy cannot read", c)
}

// decodeUpstreamBody makes resp.Body plain bytes. A gzip (x-gzip) or deflate
// body is replaced by a decoding reader, and Content-Encoding and
// Content-Length are dropped because they described the encoded bytes; the
// return value reports that. An identity body is left as it is. Any other
// coding, or more than one, is an errUnreadableEncoding.
//
// The decoder starts on the first Read, not here, so the response headers are
// not held back waiting for the upstream's first body bytes.
func decodeUpstreamBody(resp *http.Response) (decoded bool, err error) {
	values := resp.Header.Values("Content-Encoding")
	if len(values) == 0 {
		return false, nil
	}
	coding := strings.ToLower(strings.TrimSpace(strings.Join(values, ",")))
	var open func(io.Reader) (io.Reader, error)
	switch coding {
	case "", "identity":
		resp.Header.Del("Content-Encoding")
		return false, nil
	case "gzip", "x-gzip":
		open = func(r io.Reader) (io.Reader, error) { return gzip.NewReader(r) }
	case "deflate":
		open = openDeflate
	default:
		return false, errUnreadableEncoding{coding: coding}
	}
	resp.Body = &decodingBody{src: resp.Body, open: open}
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("Content-Length")
	resp.ContentLength = -1
	resp.Uncompressed = true
	return true, nil
}

// openDeflate decodes HTTP's "deflate" coding. RFC 9110 §8.4.1.2 defines it
// as the zlib format, but some servers send raw deflate under that name, so a
// body that does not start with a valid zlib header is read as raw deflate.
func openDeflate(r io.Reader) (io.Reader, error) {
	br := bufio.NewReader(r)
	h, err := br.Peek(2)
	if len(h) == 0 && errors.Is(err, io.EOF) {
		return br, nil // an empty body decodes to nothing
	}
	if len(h) == 2 && isZlibHeader(h[0], h[1]) {
		return zlib.NewReader(br)
	}
	return flate.NewReader(br), nil
}

// isZlibHeader reports whether cmf, flg form a zlib header (RFC 1950 §2.2):
// compression method 8 (deflate), a window of at most 32 KiB, and a check
// value that makes the pair a multiple of 31.
func isZlibHeader(cmf, flg byte) bool {
	return cmf&0x0f == 8 && cmf>>4 <= 7 && (uint16(cmf)<<8|uint16(flg))%31 == 0
}

// decodingBody decodes src with the decoder open returns, opened on the first
// Read. Close closes src, which also ends a Read blocked on it.
type decodingBody struct {
	src  io.ReadCloser
	open func(io.Reader) (io.Reader, error)
	dec  io.Reader
	err  error
}

func (d *decodingBody) Read(p []byte) (int, error) {
	if d.dec == nil && d.err == nil {
		d.dec, d.err = d.open(d.src)
	}
	if d.err != nil {
		return 0, d.err
	}
	return d.dec.Read(p)
}

func (d *decodingBody) Close() error { return d.src.Close() }
