package storeurl

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sort"
	"strings"
	"time"
)

// signV4 signs a bodiless request (GET, HEAD) for S3 with AWS Signature
// Version 4, using an unsigned payload.
func signV4(req *http.Request, creds Credentials, region string, now time.Time) {
	signV4Payload(req, creds, region, now, "UNSIGNED-PAYLOAD")
}

func signV4Payload(req *http.Request, creds Credentials, region string, now time.Time, payloadHash string) {
	amzDate := now.Format("20060102T150405Z")
	date := now.Format("20060102")
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	host := req.URL.Host
	req.Host = host

	signed := []string{"host", "x-amz-content-sha256", "x-amz-date"}
	if req.Header.Get("Range") != "" {
		signed = append(signed, "range")
	}
	sort.Strings(signed)
	var canonHeaders strings.Builder
	for _, h := range signed {
		v := req.Header.Get(h)
		if h == "host" {
			v = host
		}
		canonHeaders.WriteString(h + ":" + strings.TrimSpace(v) + "\n")
	}
	path := req.URL.EscapedPath()
	if path == "" {
		path = "/"
	}
	canonical := strings.Join([]string{
		req.Method,
		path,
		canonicalQuery(req),
		canonHeaders.String(),
		strings.Join(signed, ";"),
		payloadHash,
	}, "\n")

	scope := date + "/" + region + "/s3/aws4_request"
	sum := sha256.Sum256([]byte(canonical))
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(sum[:])

	key := hmacSHA256([]byte("AWS4"+creds.SecretAccessKey), date)
	key = hmacSHA256(key, region)
	key = hmacSHA256(key, "s3")
	key = hmacSHA256(key, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(key, toSign))

	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+creds.AccessKeyID+"/"+scope+
		", SignedHeaders="+strings.Join(signed, ";")+", Signature="+sig)
}

func canonicalQuery(req *http.Request) string {
	q := req.URL.Query()
	if len(q) == 0 {
		return ""
	}
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vs := q[k]
		sort.Strings(vs)
		for _, v := range vs {
			parts = append(parts, uriEncode(k, true)+"="+uriEncode(v, true))
		}
	}
	return strings.Join(parts, "&")
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

// uriEncodePath encodes each path segment the way SigV4 expects.
func uriEncodePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = uriEncode(s, true)
	}
	return strings.Join(segs, "/")
}

func uriEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			b.WriteString("%")
			b.WriteString(strings.ToUpper(hex.EncodeToString([]byte{c})))
		}
	}
	return b.String()
}
