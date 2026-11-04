package qoder

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

const checkInCampaignPath = "/sash/api/v1/me/campaigns"

func checkInTestIdentity() *MachineIdentity {
	return &MachineIdentity{MachineToken: "native-token", MachineType: "3", MachineCode: "native-code", MachineOS: "x86_64_linux", Hostname: "test-host"}
}

func TestQoderCheckInCampaignsBothRegions(t *testing.T) {
	for _, provider := range []string{ProviderIntl, ProviderCN} {
		t.Run(provider, func(t *testing.T) {
			var claims []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for name, want := range map[string]string{
					"Accept": "application/json", "User-Agent": "Qoder", "Authorization": "Bearer dt-test",
					"Cosy-ClientType": "10", "Cosy-Version": "0.4.8", "Cosy-MachineToken": "native-token",
					"Cosy-MachineType": "3", "Cosy-MachineCode": "native-code", "Cosy-MachineOS": "x86_64_linux", "Cosy-MachineHostname": "test-host",
				} {
					if r.Header.Get(name) != want {
						t.Errorf("incorrect %s header", name)
					}
				}
				for _, name := range []string{"Origin", "Referer", "X-Request-ID", "X-Machine-ID", "X-Session-ID"} {
					if r.Header.Get(name) != "" {
						t.Errorf("ordinary API header on campaign request: %s", name)
					}
				}
				if r.URL.Path == checkInCampaignPath {
					if r.Method != http.MethodGet || r.Header.Get("Content-Type") != "" {
						t.Error("campaign listing must be a bodyless GET")
					}
					_, _ = io.WriteString(w, `{"showCampaign":true,"campaigns":[
						{"campaignId":"daily","claimStatus":"CLAIMABLE","actionType":"CLAIM_BENEFIT","benefit":{"kind":"CREDITS","amount":999}},
						{"campaignId":"daily","claimStatus":"CLAIMABLE","actionType":"CLAIM_BENEFIT","benefit":{"kind":"CREDITS","amount":999}},
						{"campaignId":9007199254740993,"claimStatus":"CLAIMABLE"},
						{"campaignId":"done","claimStatus":"CLAIMED","actionType":"VIEW_DETAILS","benefit":{"kind":"CREDITS","amount":100}},
						{"campaignId":"view","claimStatus":"CLAIMABLE","actionType":"VIEW_DETAILS","benefit":{"kind":"CREDITS"}},
						{"campaignId":"coupon","claimStatus":"CLAIMABLE","actionType":"CLAIM_BENEFIT","benefit":{"kind":"REDEMPTION_CODE"}},
						{"campaignId":"pending","claimStatus":"PENDING","benefit":{"kind":"CREDITS"}}
					]}`)
					return
				}
				body, errRead := io.ReadAll(r.Body)
				if errRead != nil || string(body) != "{}" || r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
					t.Error("claims must POST an empty JSON object")
				}
				claims = append(claims, r.URL.Path)
				switch r.URL.Path {
				case checkInCampaignPath + "/daily/claim":
					_, _ = io.WriteString(w, `{"status":"CLAIMED","replayed":false,"benefit":{"kind":"CREDITS","amount":100.5}}`)
				case checkInCampaignPath + "/9007199254740993/claim":
					_, _ = io.WriteString(w, `{"status":"GRANTED","amount":"20"}`)
				default:
					t.Errorf("unexpected claim: %s", r.URL.Path)
					w.WriteHeader(500)
				}
			}))
			defer server.Close()
			service := NewService(provider, server.Client())
			service.SetEndpoint(server.URL)
			result, err := service.CheckIn(t.Context(), Credentials{UID: "uid", AccessToken: "dt-test", Realm: SiteForProvider(provider).Realm}, checkInTestIdentity())
			if err != nil || result.Status != "success" || result.Claimed != 2 || result.Already != 1 || result.Credits != 120.5 || len(claims) != 2 {
				t.Fatalf("result=%+v claims=%v err=%v", result, claims, err)
			}
		})
	}
}

func TestQoderCheckInReplayBlockedAndInvalidResponses(t *testing.T) {
	for _, tc := range []struct {
		name, body, status string
		code, already      int
		wantErr            bool
	}{
		{"replayed", `{"status":"CLAIMED","replayed":true,"benefit":{"amount":100}}`, "already", 200, 1, false},
		{"already", `{"result":"ALREADY_CLAIMED"}`, "already", 200, 1, false},
		{"conflict", `not JSON`, "already", 409, 1, false},
		{"replay-error-code", `{"errorCode":"REPLAYED"}`, "already", 400, 1, false},
		{"blocked", `{"status":"BLOCKED","replayed":true,"failureCode":"SAME_PERSON_ALREADY_CLAIMED","benefit":{"amount":100}}`, "blocked", 200, 0, false},
		{"blocked-conflict", `{"error":{"code":"SAME_PERSON_ALREADY_CLAIMED"}}`, "blocked", 409, 0, false},
		{"rejected", `{"success":false,"message":"private-secret"}`, "error", 200, 0, true},
		{"unauthorized", `{"error":"private-secret"}`, "error", 401, 0, true},
		{"claim-missing", `{}`, "error", 404, 0, true},
		{"empty", `{}`, "error", 200, 0, true},
		{"pending", `{"status":"PENDING","benefit":{"amount":100}}`, "error", 200, 0, true},
		{"bad-replay", `{"status":"CLAIMED","replayed":"false"}`, "error", 200, 0, true},
		{"non-credit", `{"status":"CLAIMED","benefit":{"kind":"COUPON"}}`, "error", 200, 0, true},
		{"bad-amount", `{"status":"CLAIMED","benefit":{"amount":"NaN"}}`, "error", 200, 0, true},
		{"negative", `{"status":"CLAIMED","amount":-1}`, "error", 200, 0, true},
		{"trailing", `{"status":"CLAIMED"} {}`, "error", 200, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path == checkInCampaignPath {
					_, _ = io.WriteString(w, `{"campaigns":[{"campaignId":"daily","claimStatus":"CLAIMABLE"}]}`)
					return
				}
				if r.URL.Path != checkInCampaignPath+"/daily/claim" {
					t.Error("claim failure must not trigger legacy fallback")
				}
				w.WriteHeader(tc.code)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			service := NewService(ProviderIntl, server.Client())
			service.SetEndpoint(server.URL)
			got, err := service.CheckIn(t.Context(), Credentials{UID: "uid", AccessToken: "private-secret"}, checkInTestIdentity())
			if (err != nil) != tc.wantErr || got.Status != tc.status || got.Already != tc.already || got.Claimed != 0 || got.Credits != 0 || calls != 2 {
				t.Fatalf("result=%+v calls=%d err=%v", got, calls, err)
			}
			if strings.Contains(fmt.Sprint(got, err), "private-secret") {
				t.Fatal("upstream secret reached result or error")
			}
		})
	}
}

func TestQoderCheckInLegacyOnlyForMissingCampaignAPI(t *testing.T) {
	for _, provider := range []string{ProviderIntl, ProviderCN} {
		for _, code := range []int{404, 405, 410, 401, 403, 429, 500, 200} {
			t.Run(fmt.Sprintf("%s/%d", provider, code), func(t *testing.T) {
				var legacy int
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == checkInCampaignPath {
						w.WriteHeader(code)
						_, _ = io.WriteString(w, `{"campaigns":[]}`)
						return
					}
					legacy++
					if r.Header.Get("Origin") != SiteForProvider(provider).Website || r.Header.Get("Cosy-ClientType") != "" {
						t.Error("legacy call used incorrect regional headers")
					}
					switch r.URL.Path {
					case "/sash/api/v1/me/daily-check-in/status":
						_, _ = io.WriteString(w, `{"status":"CLAIMABLE","rewardCredits":999}`)
					case "/sash/api/v1/me/daily-check-in/claim":
						body, _ := io.ReadAll(r.Body)
						if r.Method != http.MethodPost || string(body) != "{}" {
							t.Error("incorrect legacy claim")
						}
						_, _ = io.WriteString(w, `{"rewardCredits":100,"currentStreakDays":3}`)
					default:
						t.Errorf("unexpected path %s", r.URL.Path)
					}
				}))
				defer server.Close()
				service := NewService(provider, server.Client())
				service.SetEndpoint(server.URL)
				got, err := service.CheckIn(t.Context(), Credentials{UID: "uid", AccessToken: "dt-test"}, checkInTestIdentity())
				if code == 404 || code == 405 || code == 410 {
					if err != nil || legacy != 2 || got.Claimed != 1 || got.Credits != 100 {
						t.Fatalf("legacy result=%+v calls=%d err=%v", got, legacy, err)
					}
				} else if legacy != 0 || (err != nil) != (code != 200) {
					t.Fatalf("unexpected fallback=%d err=%v", legacy, err)
				}
			})
		}
	}
}

func TestQoderCheckInLegacyOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name, query, claim, want             string
		queryCode, claimCode, posts, already int
	}{
		{"already", `{"status":"CLAIMED"}`, "", "already", 200, 200, 0, 1},
		{"disabled", `{"status":"DISABLED"}`, "", "skipped", 200, 200, 0, 0},
		{"blocked", `{"status":"BLOCKED"}`, "", "blocked", 200, 200, 0, 0},
		{"missing-status", `{}`, "", "unavailable", 410, 200, 0, 0},
		{"missing-claim", `{"status":"CLAIMABLE"}`, `{}`, "unavailable", 200, 405, 1, 0},
		{"replay", `{"status":"CLAIMABLE"}`, `{"result":"ALREADY_CLAIMED"}`, "already", 200, 200, 1, 1},
		{"conflict", `{"status":"CLAIMABLE"}`, `conflict`, "already", 200, 409, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			posts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == checkInCampaignPath {
					w.WriteHeader(404)
				} else if r.Method == http.MethodGet {
					w.WriteHeader(tc.queryCode)
					_, _ = io.WriteString(w, tc.query)
				} else {
					posts++
					w.WriteHeader(tc.claimCode)
					_, _ = io.WriteString(w, tc.claim)
				}
			}))
			defer server.Close()
			service := NewService(ProviderCN, server.Client())
			service.SetEndpoint(server.URL)
			got, err := service.CheckIn(t.Context(), Credentials{UID: "uid", AccessToken: "dt-test"}, &MachineIdentity{})
			if err != nil || got.Status != tc.want || got.Claimed != 0 || got.Already != tc.already || got.Credits != 0 || posts != tc.posts {
				t.Fatalf("result=%+v posts=%d err=%v", got, posts, err)
			}
		})
	}
}

func TestQoderCheckInValidatesListBeforeClaims(t *testing.T) {
	for _, body := range []string{
		`{"campaigns":[{"campaignId":"ok","claimStatus":"CLAIMABLE"},null]}`,
		`{"campaigns":[{"campaignId":"ok","claimStatus":"CLAIMABLE"},{"campaignId":"ok","claimStatus":"CLAIMED"}]}`,
		`{"campaigns":[{"campaignId":"..","claimStatus":"CLAIMABLE"}]}`,
		`{"campaigns":[{"campaignId":"bad","claimStatus":"CLAIMABLE","benefit":{"kind":true}}]}`,
		`{"campaigns":[{"campaignId":"bad","claimStatus":"CLAIMABLE","actionType":false}]}`,
		`{"campaigns":null}`, `null`, `[]`, `{}`, `{"campaigns":[]} {}`,
	} {
		t.Run(body, func(t *testing.T) {
			var mutations atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					mutations.Add(1)
				}
				_, _ = io.WriteString(w, body)
			}))
			defer server.Close()
			service := NewService(ProviderCN, server.Client())
			service.SetEndpoint(server.URL)
			if _, err := service.CheckIn(t.Context(), Credentials{UID: "uid", AccessToken: "test"}, &MachineIdentity{}); err == nil || mutations.Load() != 0 {
				t.Fatalf("invalid list permitted claims: err=%v", err)
			}
		})
	}
}

func TestQoderCheckInMissingComponentAndCancellation(t *testing.T) {
	t.Setenv("QODER_RUNTIME_INFO_PATH", filepath.Join(t.TempDir(), "missing-runtime-info"))
	var calls atomic.Int32
	client := &http.Client{Transport: qoderTestTransport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		for key := range r.Header {
			if strings.HasPrefix(strings.ToLower(key), "cosy-machine") {
				t.Error("missing component produced fabricated machine headers")
			}
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"campaigns":[]}`)), Header: make(http.Header)}, nil
	})}
	creds := Credentials{UID: "uid", AccessToken: "test"}
	got, err := NewService(ProviderIntl, client).CheckIn(t.Context(), creds, nil)
	if err != nil || got.Status != "unavailable" || !strings.Contains(got.Message, "QODER_RUNTIME_INFO_PATH") || calls.Load() != 0 {
		t.Fatalf("missing identity: %+v err=%v", got, err)
	}
	got, err = NewService(ProviderCN, client).CheckIn(t.Context(), creds, nil)
	if err != nil || got.Status != "skipped" || calls.Load() != 1 {
		t.Fatalf("CN basic desktop request: %+v err=%v", got, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = NewService(ProviderCN, client).CheckIn(ctx, creds, nil); !errors.Is(err, context.Canceled) || calls.Load() != 1 {
		t.Fatal("cancelled check-in made a request")
	}
	if _, err = NativeMachineIdentity(ctx, ProviderIntl, "uid"); !errors.Is(err, context.Canceled) {
		t.Fatal("native sampling ignored cancellation")
	}
}

// The test binary also acts as a local runtime-info fixture. No installed
// component, account directory or OS identity seed is inspected by these tests.
func TestMain(m *testing.M) {
	if os.Getenv("QODER_CHECKIN_RUNTIME_STUB") == "1" {
		args := os.Args[1:]
		input, _ := io.ReadAll(os.Stdin)
		want := os.Getenv("QODER_CHECKIN_RUNTIME_REGION")
		if len(args) == 0 || args[0] != want {
			os.Exit(2)
		}
		if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
			if len(args) != 2 || args[1] != "--account-stdin" || string(input) != "{\"account\":\"test-uid\"}\n" {
				os.Exit(3)
			}
		} else if len(args) != 1 || len(input) != 0 {
			os.Exit(4)
		}
		output := os.Getenv("QODER_CHECKIN_RUNTIME_OUTPUT")
		if output == "oversized" {
			output = strings.Repeat("x", 65537)
		}
		if output == "oversized-valid" {
			output = `{"machineToken":"native-token","machineType":"3","machineCode":"native-code"}` + "\n" + strings.Repeat("x", 65537)
		}
		_, _ = io.WriteString(os.Stdout, output)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestQoderCheckInNativeRuntimeContract(t *testing.T) {
	for _, goos := range []string{"windows", "darwin", "linux"} {
		for _, provider := range []string{ProviderCN, ProviderIntl} {
			for _, uid := range []string{"", "uid with \"quotes\""} {
				args, stdin := runtimeInfoInput(provider, uid, goos)
				region := "3"
				if provider == ProviderCN {
					region = "0"
				}
				want := []string{region}
				if uid != "" && goos != "linux" {
					want = append(want, "--account-stdin")
					var decoded map[string]string
					if json.Unmarshal(stdin, &decoded) != nil || decoded["account"] != uid || stdin[len(stdin)-1] != '\n' {
						t.Fatal("account stdin contract differs")
					}
				} else if len(stdin) != 0 {
					t.Fatal("Linux or empty account must send EOF")
				}
				if !reflect.DeepEqual(args, want) {
					t.Fatalf("args=%v want=%v", args, want)
				}
			}
		}
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("QODER_RUNTIME_INFO_PATH", exe)
	t.Setenv("QODER_CHECKIN_RUNTIME_STUB", "1")
	for _, provider := range []string{ProviderCN, ProviderIntl} {
		region := "3"
		if provider == ProviderCN {
			region = "0"
		}
		t.Setenv("QODER_CHECKIN_RUNTIME_REGION", region)
		for _, output := range []string{
			`{"machineToken":"native-token","machineType":"3","machineCode":"native-code"}` + "\nignored diagnostics\n",
			`{"machineToken":"incomplete"}`, `invalid secret diagnostics`, "oversized", "oversized-valid",
		} {
			t.Setenv("QODER_CHECKIN_RUNTIME_OUTPUT", output)
			ident, errNative := NativeMachineIdentity(t.Context(), provider, "test-uid")
			if strings.HasPrefix(output, `{"machineToken":"native-token"`) {
				if errNative != nil || ident == nil || ident.MachineToken != "native-token" {
					t.Fatalf("native fixture failed: %v", errNative)
				}
			} else if !errors.Is(errNative, ErrMachineIdentityUnavailable) || ident != nil {
				t.Fatal("invalid component output accepted")
			}
		}
	}
	t.Setenv("QODER_RUNTIME_INFO_PATH", "runtime-info")
	if _, err = NativeMachineIdentity(t.Context(), ProviderIntl, "test-uid"); !errors.Is(err, ErrMachineIdentityUnavailable) {
		t.Fatal("relative executable path accepted")
	}
}

func TestQoderCheckInMetadataIdentityAndHeaderSafety(t *testing.T) {
	auth := NewAuthRecord(ProviderIntl, Credentials{UID: "uid"})
	for _, raw := range []any{nil, "invalid", map[string]any{"machineToken": "only-token"}, map[string]any{"machineToken": "token\r\nx: secret", "machineType": "3", "machineCode": "code"}} {
		auth.Metadata[MachineIdentityMetadataKey] = raw
		if MachineIdentityFromAuth(auth) != nil {
			t.Fatal("malformed native identity accepted")
		}
	}
	auth.Metadata[MachineIdentityMetadataKey] = map[string]any{"machineToken": "real", "machineType": "3", "machineCode": "code", "hostname": "测试主机"}
	ident := MachineIdentityFromAuth(auth)
	if ident == nil || ident.MachineToken != "real" || ident.Hostname == "" || !safeIdentityHeader(ident.Hostname) {
		t.Fatal("native metadata or hostname normalization lost")
	}
	if got := desktopHostname(strings.Repeat("x", 120)); len(got) > 96 || got == "" {
		t.Fatal("hostname length not bounded")
	}
}
