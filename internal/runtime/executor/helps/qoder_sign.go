package helps

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/qoder"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

const qoderAlphabet = "_doRTgHZBKcGVjlvpC,@aFSx#DPuNJme&i*MzLOEn)sUrthbf%Y^w.(kIQyXqWA!"
const qoderCosyVersion = "1.1.75"
const qoderDefaultUserType = "personal_professional_trial"
const qoderPublicKey = `-----BEGIN PUBLIC KEY-----
MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDA8iMH5c02LilrsERw9t6Pv5Nc
4k6Pz1EaDicBMpdpxKduSZu5OANqUq8er4GM95omAGIOPOh+Nx0spthYA2BqGz+l
6HRkPJ7S236FZz73In/KVuLnwI8JJ2CbuJap8kvheCCZpmAWpb/cPx/3Vr/J6I17
XcW+ML9FoCI6AOvOzwIDAQAB
-----END PUBLIC KEY-----`

type qoderCosyIdentity struct {
	version, userAgent, clientType, machineOS string
	sendOS                                    bool
}

// Resolve once per signature so payload and headers always use the same version.
// Names and defaults match qoder_sign.py; the OS switch is QD_COSY_OS_HEADERS.
func qoderIdentityFromEnv() qoderCosyIdentity {
	value := func(name, fallback string) string {
		if text := strings.TrimSpace(os.Getenv(name)); text != "" {
			return text
		}
		return fallback
	}
	identity := qoderCosyIdentity{
		version:    value("QD_COSY_VERSION", qoderCosyVersion),
		userAgent:  value("QD_COSY_UA", "undici"),
		clientType: value("QD_COSY_CLIENTTYPE", "5"),
		machineOS:  qoderMachineOS(strings.TrimSpace(os.Getenv("QD_COSY_MACHINE_OS")), runtime.GOOS, runtime.GOARCH),
		sendOS:     true,
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("QD_COSY_OS_HEADERS"))) {
	case "0", "off", "false", "no", "disable", "disabled":
		identity.sendOS = false
	}
	return identity
}

func qoderMachineOS(override, goos, goarch string) string {
	if strings.EqualFold(override, "auto") {
		arch := "x86_64"
		if goarch == "arm64" || goarch == "aarch64" {
			arch = "aarch64"
		}
		platform := "linux"
		if goos == "windows" {
			platform = "win32"
		} else if goos == "darwin" {
			platform = "darwin"
		}
		return arch + "_" + platform
	}
	if override != "" {
		return override
	}
	// Keep the upstream compatibility default even on macOS and ARM hosts.
	if goos == "windows" {
		return "x86_64_win32"
	}
	return "x86_64_linux"
}

// EncodeQoderBody applies the gateway's base64 alphabet and tail/middle/head permutation.
func EncodeQoderBody(raw []byte) []byte {
	encoded := base64.NewEncoding(qoderAlphabet).WithPadding('$').EncodeToString(raw)
	n, a := len(encoded), len(encoded)/3
	return []byte(encoded[n-a:] + encoded[a:n-a] + encoded[:a])
}

type qoderSession struct {
	tempKey   []byte
	key, info string
}

// A session is deliberately private to one request. Every credentials field is
// therefore fresh, and neither regional providers nor rotated tokens share state.
func newQoderSession(creds qoder.Credentials) (*qoderSession, error) {
	id, err := qoderUUID()
	if err != nil {
		return nil, err
	}
	key := []byte(strings.ReplaceAll(id, "-", "")[:16])
	wrapped, err := qoderWrapKey(key, rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("qoder: wrap session key: %w", err)
	}
	userType := creds.UserType
	if userType == "" {
		userType = qoderDefaultUserType
	}
	identity := qoderSortedStrings(map[string]string{
		"name": creds.Nickname, "aid": creds.UID, "uid": creds.UID, "yx_uid": "",
		"organization_id": creds.OrganizationID, "organization_name": creds.OrganizationName,
		"user_type": userType, "security_oauth_token": creds.AccessToken, "refresh_token": creds.RefreshToken,
	})
	sealed, err := qoderEncryptInfo(identity, key)
	if err != nil {
		return nil, err
	}
	return &qoderSession{tempKey: key, key: base64.StdEncoding.EncodeToString(wrapped), info: base64.StdEncoding.EncodeToString(sealed)}, nil
}

func qoderWrapKey(key []byte, random io.Reader) ([]byte, error) {
	block, _ := pem.Decode([]byte(qoderPublicKey))
	if block == nil {
		return nil, fmt.Errorf("qoder: invalid protocol public key")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	public, ok := parsed.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("qoder: protocol key is not RSA")
	}
	return rsa.EncryptPKCS1v15(random, public, key)
}

func qoderEncryptInfo(plain, key []byte) ([]byte, error) {
	if len(key) != aes.BlockSize {
		return nil, fmt.Errorf("qoder: invalid AES session key length")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	pad := aes.BlockSize - len(plain)%aes.BlockSize
	padded := make([]byte, len(plain)+pad)
	copy(padded, plain)
	for i := len(plain); i < len(padded); i++ {
		padded[i] = byte(pad)
	}
	cipher.NewCBCEncrypter(block, key).CryptBlocks(padded, padded)
	return padded, nil
}

// qoderSortedStrings matches Python ensure_ascii=False, including U+2028/U+2029.
// Both signed objects contain only strings; no float or nested-map canonicalization is needed.
func qoderSortedStrings(values map[string]string) []byte {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var out strings.Builder
	quote := func(value string) {
		out.WriteByte('"')
		for _, r := range value {
			switch r {
			case '"', '\\':
				out.WriteByte('\\')
				out.WriteRune(r)
			case '\b':
				out.WriteString(`\b`)
			case '\f':
				out.WriteString(`\f`)
			case '\n':
				out.WriteString(`\n`)
			case '\r':
				out.WriteString(`\r`)
			case '\t':
				out.WriteString(`\t`)
			default:
				if r < 0x20 {
					fmt.Fprintf(&out, `\u%04x`, r)
				} else {
					out.WriteRune(r)
				}
			}
		}
		out.WriteByte('"')
	}
	out.WriteByte('{')
	for i, key := range keys {
		if i > 0 {
			out.WriteByte(',')
		}
		quote(key)
		out.WriteByte(':')
		quote(values[key])
	}
	out.WriteByte('}')
	return []byte(out.String())
}

func qoderUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("qoder: random ID: %w", err)
	}
	b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:], nil
}

func qoderDigest(payload, key, date string, encodedBody []byte, path string) string {
	path = strings.TrimPrefix(path, "/algo")
	h := md5.New()
	_, _ = io.WriteString(h, payload+"\n"+key+"\n"+date+"\n")
	_, _ = h.Write(encodedBody)
	_, _ = io.WriteString(h, "\n"+path)
	return hex.EncodeToString(h.Sum(nil))
}

// SignQoderRequest signs the final encoded bytes for the Cosy model or inference
// gateway. Callers must construct Body before signing and never mutate it after.
// This is not the signer for ordinary Qoder OpenAPI/account requests.
func SignQoderRequest(req *http.Request, auth *coreauth.Auth, encodedBody []byte, model string, sse bool) error {
	if req == nil || req.URL == nil {
		return fmt.Errorf("qoder: missing request URL")
	}
	if auth == nil || (auth.Provider != qoder.ProviderIntl && auth.Provider != qoder.ProviderCN) {
		return fmt.Errorf("qoder: missing or incompatible credential provider")
	}
	creds := qoder.CredentialsFromAuth(auth)
	wantRealm := "intl"
	if auth.Provider == qoder.ProviderCN {
		wantRealm = "cn"
	}
	if creds.Realm != "" && creds.Realm != wantRealm {
		return fmt.Errorf("qoder: credential region does not match provider")
	}
	if creds.UID == "" || creds.AccessToken == "" {
		return fmt.Errorf("qoder: missing account credentials")
	}
	if req.GetBody != nil {
		body, err := req.GetBody()
		if err != nil {
			return fmt.Errorf("qoder: inspect signing body: %w", err)
		}
		actual, errRead := io.ReadAll(body)
		errClose := body.Close()
		if errRead != nil {
			return errRead
		}
		if errClose != nil {
			return errClose
		}
		if !bytes.Equal(actual, encodedBody) {
			return fmt.Errorf("qoder: signing body does not match request")
		}
	} else if req.Body != nil {
		actual, err := io.ReadAll(req.Body)
		if err != nil {
			return fmt.Errorf("qoder: inspect signing body: %w", err)
		}
		if errClose := req.Body.Close(); errClose != nil {
			return errClose
		}
		req.Body = io.NopCloser(bytes.NewReader(actual))
		if !bytes.Equal(actual, encodedBody) {
			return fmt.Errorf("qoder: signing body does not match request")
		}
	} else if len(encodedBody) != 0 {
		return fmt.Errorf("qoder: signing body is absent from request")
	}
	session, err := newQoderSession(creds)
	if err != nil {
		return err
	}
	requestID, err := qoderUUID()
	if err != nil {
		return err
	}
	identity := qoderIdentityFromEnv()
	payload := base64.StdEncoding.EncodeToString(qoderSortedStrings(map[string]string{
		"cosyVersion": identity.version, "ideVersion": "", "info": session.info, "requestId": requestID, "version": "v1",
	}))
	date := strconv.FormatInt(time.Now().Unix(), 10)
	path := req.URL.EscapedPath()
	if path == "" {
		path = "/"
	}
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	// Remove prior Cosy identity when a caller reuses a request for another account.
	for key := range req.Header {
		if strings.HasPrefix(strings.ToLower(key), "cosy-") {
			delete(req.Header, key)
		}
	}
	for _, key := range []string{"X-Model-Key", "X-Model-Source", "Cache-Control"} {
		req.Header.Del(key)
	}
	// OAuth MachineID identifies a login attempt, not the inference device.
	machineID := qoder.DeriveID(creds.UID, "machine")
	for key, value := range map[string]string{
		"Cosy-Data-Policy": "agree", "Content-Type": "application/json", "Cosy-Business-Product": "cli",
		"Cosy-Business-Type": "agent", "Cosy-Scene": "assistant", "Cosy-Clienttype": identity.clientType, "Cosy-Date": date,
		"Cosy-User": creds.UID, "Cosy-Key": session.key, "Accept": "application/json", "Accept-Encoding": "identity",
		"Authorization": "Bearer COSY." + payload + "." + qoderDigest(payload, session.key, date, encodedBody, path),
		"Cosy-Version":  identity.version, "Cosy-Machineid": machineID, "Cosy-Machinetoken": qoder.MachineToken(creds.UID),
		"Cosy-Machinetype": qoder.MachineType(creds.UID), "Cosy-Clientip": "169.254.198.161", "Login-Version": "v2",
		"User-Agent": identity.userAgent,
	} {
		req.Header.Set(key, value)
	}
	if identity.sendOS {
		req.Header.Set("Cosy-Machineos", identity.machineOS)
		if sse {
			hostname := md5.Sum([]byte("hostname:" + creds.UID))
			req.Header.Set("Cosy-Machinehostname", "DESKTOP-"+strings.ToUpper(hex.EncodeToString(hostname[:])[:7]))
		}
	}
	if sse {
		req.Header.Set("Cache-Control", "no-cache")
		req.Header.Set("Accept", "text/event-stream")
	}
	if model != "" {
		req.Header.Set("X-Model-Key", model)
		req.Header.Set("X-Model-Source", "system")
	}
	return nil
}

// QoderSafeHeaders strips encrypted credentials too: Cosy info/key must never
// appear in logs or downstream headers, even though they are not plain tokens.
func QoderSafeHeaders(headers http.Header) http.Header {
	out := headers.Clone()
	if out == nil {
		out = make(http.Header)
	}
	for key := range out {
		low := strings.ToLower(key)
		if strings.HasPrefix(low, "cosy-") || low == "authorization" || low == "proxy-authorization" || low == "cookie" || low == "set-cookie" {
			delete(out, key)
		}
	}
	return out
}
