package helps

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/qoder"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// These vectors were produced by qoder_sign.py at upstream 45e76f9.
func TestQoderEncodeVectors(t *testing.T) {
	for _, test := range []struct{ raw, encoded string }{
		{"", ""}, {"a", "$p$#"}, {"ab", "$SB#"}, {"abc", "MSK#"},
		{"hello", "q$FruHPH"}, {"你好🙂", "D&&$$PSWqKAQG(&Q"},
	} {
		if got := string(EncodeQoderBody([]byte(test.raw))); got != test.encoded {
			t.Errorf("encode(%q) = %q, want %q", test.raw, got, test.encoded)
		}
	}
}

func TestQoderCryptoVectors(t *testing.T) {
	// Go 1.26 otherwise ignores the supplied Reader. Scope the compatibility
	// setting to this vector test; production always uses crypto/rand.Reader.
	t.Setenv("GODEBUG", os.Getenv("GODEBUG")+",cryptocustomrand=1")
	key := []byte("0123456789abcdef")
	for plain, want := range map[string]string{
		"Cosy test vector": "02532b61a0081f885437eb5e4edf32522da00448fc68e9483c37a519fe88cc4c",
		"":                 "ed47fee0545c3fa7dd070d44b86e98d9",
	} {
		got, err := qoderEncryptInfo([]byte(plain), key)
		if err != nil || hex.EncodeToString(got) != want {
			t.Fatalf("AES vector: %x, %v", got, err)
		}
	}
	wrapped, err := qoderWrapKey(key, bytes.NewReader(bytes.Repeat([]byte{0x42}, 1024)))
	const wantRSA = "jlzTfvkw7WeusLt1fNKdCC7pmd41JuCnr0iWCQMq2kgn3o/Rul8viJfhwoA3f9WXMKOEHMPZnzfbe20jO1hw5Cjd/tqI5w8fGQsXQ6F2VTazPxx13f218OS/Riggus4kM7mVd249IDz6QZhf1i6CRf/zy5GjMV0DwuWbqYgHYdg="
	if err != nil || base64.StdEncoding.EncodeToString(wrapped) != wantRSA {
		t.Fatalf("RSA PKCS1v15 vector mismatch: got %s, %v", base64.StdEncoding.EncodeToString(wrapped), err)
	}
	if got := qoderDigest("PAYLOAD", "COSYKEY", "1700000000", []byte("ENCODED"), "/algo/api/v2/model/list"); got != "90309d407ed389ebf7d700dbbb6ec364" {
		t.Fatalf("MD5 vector = %s", got)
	}
	if qoderDigest("a", "b", "1", nil, "/algo") != qoderDigest("a", "b", "1", nil, "") {
		t.Fatal("/algo prefix removal differs from Python")
	}
}

func TestQoderIdentityAndHeaders(t *testing.T) {
	t.Setenv("QD_DEVICE_ID_LEGACY", "0")
	for _, key := range []string{"QD_COSY_VERSION", "QD_COSY_UA", "QD_COSY_CLIENTTYPE", "QD_COSY_OS_HEADERS", "QD_COSY_MACHINE_OS"} {
		t.Setenv(key, "")
	}
	creds := qoder.Credentials{UID: "user-123", Nickname: "用户<&>", AccessToken: "access-test", RefreshToken: "refresh-test", OrganizationID: "org", OrganizationName: "team"}
	session, err := newQoderSession(creds)
	if err != nil {
		t.Fatal(err)
	}
	if len(session.tempKey) != 16 || session.tempKey[12] != '4' || strings.Trim(string(session.tempKey), "0123456789abcdef") != "" {
		t.Fatal("AES key is not UUIDv4 hex[:16] ASCII")
	}
	sealed, _ := base64.StdEncoding.DecodeString(session.info)
	block, _ := aes.NewCipher(session.tempKey)
	cipher.NewCBCDecrypter(block, session.tempKey).CryptBlocks(sealed, sealed)
	plain := sealed[:len(sealed)-int(sealed[len(sealed)-1])]
	const want = `{"aid":"user-123","name":"用户<&>","organization_id":"org","organization_name":"team","refresh_token":"refresh-test","security_oauth_token":"access-test","uid":"user-123","user_type":"personal_professional_trial","yx_uid":""}`
	if string(plain) != want {
		t.Fatalf("identity is not the sorted nine-field object: %s", plain)
	}
	if got := string(qoderSortedStrings(map[string]string{"z": "\u2028\u2029", "a": "\n\t\"\\\x00"})); got != "{\"a\":\"\\n\\t\\\"\\\\\\u0000\",\"z\":\"\u2028\u2029\"}" {
		t.Fatalf("Python JSON escaping mismatch: %s", got)
	}
	for _, provider := range []string{qoder.ProviderIntl, qoder.ProviderCN} {
		for _, sse := range []bool{false, true} {
			auth := &coreauth.Auth{Provider: provider}
			qoder.ApplyCredentials(auth, creds)
			body := EncodeQoderBody([]byte(`{"text":"hello"}`))
			method, model := http.MethodPost, "model-key"
			if !sse {
				method, model, body = http.MethodGet, "", nil
			}
			req, _ := http.NewRequest(method, "https://gateway.example/algo/api/v2/model/list?Encode=1", bytes.NewReader(body))
			if err := SignQoderRequest(req, auth, body, model, sse); err != nil {
				t.Fatal(err)
			}
			for key, value := range map[string]string{
				"Cosy-Data-Policy": "agree", "Content-Type": "application/json", "Cosy-Business-Product": "cli", "Cosy-Business-Type": "agent", "Cosy-Scene": "assistant", "Cosy-Clienttype": "5", "Cosy-Version": "1.1.75", "Cosy-User": "user-123", "Accept-Encoding": "identity", "Login-Version": "v2", "User-Agent": "undici", "Cosy-Clientip": "169.254.198.161",
				"Cosy-Machineid": "7ed99a5c-40e7-4ee3-adcb-6a309445d0bb", "Cosy-Machinetoken": "bfUFAusUsTGQoLEI1AbEslQllzQzYJm_9byTbMg1eBJ", "Cosy-Machinetype": "c3f3ab7901a74dbaa2",
			} {
				if req.Header.Get(key) != value {
					t.Errorf("%s = %q", key, req.Header.Get(key))
				}
			}
			if (req.Header.Get("Accept") == "text/event-stream") != sse || (req.Header.Get("Cosy-Machinehostname") == "DESKTOP-EFB3DCC") != sse || (req.Header.Get("Cache-Control") == "no-cache") != sse {
				t.Fatal("SSE-only headers leaked to model GET")
			}
			if req.Header.Get("Cosy-Machineos") == "" || req.Header.Get("X-Model-Key") != model || (req.Header.Get("X-Model-Source") == "system") != sse {
				t.Fatal("missing platform/model headers")
			}
			parts := strings.Split(strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer COSY."), ".")
			if len(parts) != 2 || parts[1] != qoderDigest(parts[0], req.Header.Get("Cosy-Key"), req.Header.Get("Cosy-Date"), body, req.URL.Path) {
				t.Fatal("signature does not cover the final body and path")
			}
			payload, _ := base64.StdEncoding.DecodeString(parts[0])
			var info map[string]string
			if err := json.Unmarshal(payload, &info); err != nil || len(info) != 5 || info["version"] != "v1" || info["cosyVersion"] != "1.1.75" || len(info["requestId"]) != 36 {
				t.Fatalf("invalid bearer payload: %s", payload)
			}
			if req.Body != nil {
				sent, _ := io.ReadAll(req.Body)
				if !bytes.Equal(sent, body) {
					t.Fatal("signer mutated body")
				}
			}
			oldKey := req.Header.Get("Cosy-Key")
			creds.AccessToken = "rotated"
			qoder.ApplyCredentials(auth, creds)
			if err := SignQoderRequest(req, auth, body, model, sse); err != nil || req.Header.Get("Cosy-Key") == oldKey {
				t.Fatal("session reused after credential rotation")
			}
		}
	}
}

func TestQoderInferenceDeviceIgnoresOAuthMachineID(t *testing.T) {
	t.Setenv("QD_DEVICE_ID_LEGACY", "0")
	t.Setenv("QD_COSY_OS_HEADERS", "1")
	for _, provider := range []string{qoder.ProviderIntl, qoder.ProviderCN} {
		var previous http.Header
		for _, loginMachine := range []string{"login-machine-one", "login-machine-two", ""} {
			auth := &coreauth.Auth{Provider: provider}
			qoder.ApplyCredentials(auth, qoder.Credentials{UID: "same-user", AccessToken: "token-" + loginMachine, MachineID: loginMachine})
			req, _ := http.NewRequest(http.MethodPost, "https://gateway.example/algo/test", nil)
			if err := SignQoderRequest(req, auth, nil, "m", true); err != nil {
				t.Fatal(err)
			}
			if req.Header.Get("Cosy-Machineid") != qoder.DeriveID("same-user", "machine") {
				t.Fatal("OAuth login MachineID changed inference identity")
			}
			if previous != nil {
				for _, key := range []string{"Cosy-Machineid", "Cosy-Machinetoken", "Cosy-Machinetype", "Cosy-Machinehostname"} {
					if req.Header.Get(key) != previous.Get(key) {
						t.Fatalf("%s changed after relogin", key)
					}
				}
				if req.Header.Get("Cosy-Key") == previous.Get("Cosy-Key") {
					t.Fatal("session was reused after relogin")
				}
			}
			previous = req.Header.Clone()
		}
	}
}

func TestQoderCosyEnvironmentOverrides(t *testing.T) {
	auth := &coreauth.Auth{Provider: qoder.ProviderIntl, Metadata: map[string]any{"uid": "u", "access_token": "a"}}
	for _, test := range []struct{ version, ua, client, os, wantVersion, wantUA, wantClient, wantOS string }{
		{" 1.2.3 ", " custom-agent ", " 10 ", " custom_os ", "1.2.3", "custom-agent", "10", "custom_os"},
		{"  ", "\t", "", " AuTo ", qoderCosyVersion, "undici", "5", qoderMachineOS("auto", runtime.GOOS, runtime.GOARCH)},
	} {
		t.Run(test.wantVersion, func(t *testing.T) {
			t.Setenv("QD_COSY_VERSION", test.version)
			t.Setenv("QD_COSY_UA", test.ua)
			t.Setenv("QD_COSY_CLIENTTYPE", test.client)
			t.Setenv("QD_COSY_MACHINE_OS", test.os)
			for _, disabled := range []string{"", "1", "0", " OFF ", "false", "no", "disable", "disabled"} {
				t.Setenv("QD_COSY_OS_HEADERS", disabled)
				for _, sse := range []bool{true, false} {
					req, _ := http.NewRequest(http.MethodGet, "https://gateway.example/algo/model", nil)
					req.Header.Set("Cosy-Machineos", "stale")
					req.Header.Set("Cosy-Machinehostname", "stale")
					if err := SignQoderRequest(req, auth, nil, "", sse); err != nil {
						t.Fatal(err)
					}
					parts := strings.Split(strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer COSY."), ".")
					raw, _ := base64.StdEncoding.DecodeString(parts[0])
					var payload map[string]string
					_ = json.Unmarshal(raw, &payload)
					if req.Header.Get("Cosy-Version") != test.wantVersion || payload["cosyVersion"] != test.wantVersion || req.Header.Get("User-Agent") != test.wantUA || req.Header.Get("Cosy-Clienttype") != test.wantClient {
						t.Fatal("environment identity was not shared by payload and headers")
					}
					enabled := disabled == "" || disabled == "1"
					if enabled && req.Header.Get("Cosy-Machineos") != test.wantOS || !enabled && req.Header.Get("Cosy-Machineos") != "" || (req.Header.Get("Cosy-Machinehostname") != "") != (enabled && sse) {
						t.Fatalf("OS headers mismatch for %q sse=%v", disabled, sse)
					}
				}
			}
		})
	}
	for _, test := range []struct{ override, os, arch, want string }{
		{"", "darwin", "arm64", "x86_64_linux"}, {"", "windows", "arm64", "x86_64_win32"}, {"auto", "darwin", "arm64", "aarch64_darwin"}, {"auto", "windows", "amd64", "x86_64_win32"}, {"auto", "linux", "arm64", "aarch64_linux"}, {"auto", "freebsd", "386", "x86_64_linux"},
	} {
		if got := qoderMachineOS(test.override, test.os, test.arch); got != test.want {
			t.Fatalf("OS identity: %s, want %s", got, test.want)
		}
	}
}

func TestQoderSignerPathAndAllCredentialFields(t *testing.T) {
	base := qoder.Credentials{UID: "uid", AccessToken: "access", RefreshToken: "refresh", PersonalToken: "pat", Nickname: "name", Email: "mail", UserType: "personal_standard", OrganizationID: "org", OrganizationName: "team", Realm: "intl", Domain: "qoder.com", MachineID: "machine", ExpiresAt: 1700000000}
	seen := make(map[string]bool)
	for _, provider := range []string{qoder.ProviderIntl, qoder.ProviderCN} {
		for _, change := range []func(*qoder.Credentials){
			func(*qoder.Credentials) {}, func(c *qoder.Credentials) { c.AccessToken += "2" }, func(c *qoder.Credentials) { c.RefreshToken += "2" }, func(c *qoder.Credentials) { c.PersonalToken += "2" }, func(c *qoder.Credentials) { c.UID += "2" }, func(c *qoder.Credentials) { c.Nickname += "2" }, func(c *qoder.Credentials) { c.Email += "2" }, func(c *qoder.Credentials) { c.UserType += "2" }, func(c *qoder.Credentials) { c.OrganizationID += "2" }, func(c *qoder.Credentials) { c.OrganizationName += "2" }, func(c *qoder.Credentials) { c.Realm += "2" }, func(c *qoder.Credentials) { c.Domain += "2" }, func(c *qoder.Credentials) { c.MachineID += "2" }, func(c *qoder.Credentials) { c.ExpiresAt++ },
		} {
			creds := base
			if provider == qoder.ProviderCN {
				creds.Realm = "cn"
			}
			change(&creds)
			auth := &coreauth.Auth{Provider: provider}
			qoder.ApplyCredentials(auth, creds)
			req, _ := http.NewRequest(http.MethodGet, "https://gateway.example/algo/a%2Fb?excluded=yes", nil)
			if strings.HasSuffix(creds.Realm, "2") {
				if err := SignQoderRequest(req, auth, nil, "", false); err == nil {
					t.Fatal("signed incompatible credential realm")
				}
				continue
			}
			if err := SignQoderRequest(req, auth, nil, "", false); err != nil {
				t.Fatal(err)
			}
			key := req.Header.Get("Cosy-Key")
			if seen[key] {
				t.Fatal("session reused across credentials or provider")
			}
			seen[key] = true
			parts := strings.Split(strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer COSY."), ".")
			if parts[1] != qoderDigest(parts[0], key, req.Header.Get("Cosy-Date"), nil, "/algo/a%2Fb") {
				t.Fatal("signature changed URL-escaped path or included query")
			}
		}
	}
}

func TestQoderSignerRejectsInvalidInput(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://gateway.example/algo/path", strings.NewReader("body"))
	for _, auth := range []*coreauth.Auth{nil, {Provider: "qoder"}, {Provider: "other", Metadata: map[string]any{"uid": "u", "access_token": "a"}}} {
		if err := SignQoderRequest(req, auth, []byte("body"), "m", true); err == nil {
			t.Fatal("accepted invalid credentials")
		}
	}
	auth := &coreauth.Auth{Provider: "qoder", Metadata: map[string]any{"uid": "u", "access_token": "a"}}
	if err := SignQoderRequest(req, auth, []byte("changed"), "m", true); err == nil {
		t.Fatal("signed bytes differ from request body")
	}
}
