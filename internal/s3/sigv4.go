package s3

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

type CredentialsResolver interface {
	Lookup(accessKey string) (secret string, bucket string, readOnly bool, err error)
}

type AuthResult struct {
	AccessKey string
	Bucket    string
	ReadOnly  bool
}

func VerifySigV4(r *http.Request, resolver CredentialsResolver) (AuthResult, error) {
	a := r.Header.Get("Authorization")
	if !strings.HasPrefix(a, "AWS4-HMAC-SHA256 ") {
		return AuthResult{}, fmt.Errorf("missing auth")
	}
	parts := parseAuthFields(strings.TrimPrefix(a, "AWS4-HMAC-SHA256 "))
	cred := parts["Credential"]
	signed := parts["SignedHeaders"]
	sig := parts["Signature"]
	if cred == "" || signed == "" || sig == "" {
		return AuthResult{}, fmt.Errorf("malformed auth")
	}
	credParts := strings.Split(cred, "/")
	if len(credParts) != 5 {
		return AuthResult{}, fmt.Errorf("bad credential scope")
	}
	accessKey := credParts[0]
	date := credParts[1]
	region := credParts[2]
	service := credParts[3]
	if service != "s3" {
		return AuthResult{}, fmt.Errorf("service must be s3")
	}
	amzDate := r.Header.Get("X-Amz-Date")
	if amzDate == "" {
		return AuthResult{}, fmt.Errorf("missing x-amz-date")
	}
	payloadHash := r.Header.Get("X-Amz-Content-Sha256")
	if payloadHash == "" {
		payloadHash = "UNSIGNED-PAYLOAD"
	}
	secret, bucket, readOnly, err := resolver.Lookup(accessKey)
	if err != nil {
		return AuthResult{}, fmt.Errorf("invalid access key")
	}
	canonReq, err := canonicalRequest(r, signed, payloadHash)
	if err != nil {
		return AuthResult{}, err
	}
	h := sha256.Sum256([]byte(canonReq))
	scope := fmt.Sprintf("%s/%s/%s/aws4_request", date, region, service)
	strToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(h[:])
	kDate := hmacSHA256([]byte("AWS4"+secret), date)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, service)
	kSign := hmacSHA256(kService, "aws4_request")
	expected := hex.EncodeToString(hmacSHA256(kSign, strToSign))
	if subtle.ConstantTimeCompare([]byte(expected), []byte(sig)) != 1 {
		return AuthResult{}, fmt.Errorf("signature mismatch")
	}
	return AuthResult{AccessKey: accessKey, Bucket: bucket, ReadOnly: readOnly}, nil
}

func parseAuthFields(s string) map[string]string {
	m := map[string]string{}
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		kv := strings.SplitN(p, "=", 2)
		if len(kv) != 2 {
			continue
		}
		m[kv[0]] = kv[1]
	}
	return m
}

func canonicalRequest(r *http.Request, signedHeaders, payloadHash string) (string, error) {
	hdrs := strings.Split(strings.ToLower(signedHeaders), ";")
	sort.Strings(hdrs)
	canonHeaders := strings.Builder{}
	for _, k := range hdrs {
		v := strings.Join(r.Header.Values(http.CanonicalHeaderKey(k)), ",")
		if k == "host" {
			v = r.Host
		}
		v = strings.Join(strings.Fields(v), " ")
		canonHeaders.WriteString(k)
		canonHeaders.WriteString(":")
		canonHeaders.WriteString(v)
		canonHeaders.WriteString("\n")
	}
	canonURI := encodePath(r.URL.EscapedPath())
	canonQ := canonicalQuery(r.URL)
	return r.Method + "\n" + canonURI + "\n" + canonQ + "\n" + canonHeaders.String() + "\n" + strings.Join(hdrs, ";") + "\n" + payloadHash, nil
}

func encodePath(p string) string {
	if p == "" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return p
}

func canonicalQuery(u *url.URL) string {
	if u.RawQuery == "" {
		return ""
	}
	vals, _ := url.ParseQuery(u.RawQuery)
	type kv struct{ k, v string }
	out := []kv{}
	for k, vs := range vals {
		escK := awsEncode(k)
		sort.Strings(vs)
		for _, v := range vs {
			out = append(out, kv{escK, awsEncode(v)})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].k == out[j].k {
			return out[i].v < out[j].v
		}
		return out[i].k < out[j].k
	})
	parts := make([]string, 0, len(out))
	for _, p := range out {
		parts = append(parts, p.k+"="+p.v)
	}
	return strings.Join(parts, "&")
}

func awsEncode(s string) string {
	e := url.QueryEscape(s)
	e = strings.ReplaceAll(e, "+", "%20")
	e = strings.ReplaceAll(e, "*", "%2A")
	e = strings.ReplaceAll(e, "%7E", "~")
	return e
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}
