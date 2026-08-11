package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"
)

// stubK8sClient serves a fixed set of Ingress hosts, standing in for the
// cluster so syncRules can be exercised without one.
type stubK8sClient struct {
	hosts []string
	err   error
}

func (s *stubK8sClient) GetIngressHosts() ([]string, error) { return s.hosts, s.err }

func TestGenerateSmartRule(t *testing.T) {
	tests := []struct {
		name       string
		hosts      []string
		rootDomain string
		ip         string
		wantRules  []string
	}{
		{
			name:       "Single Host",
			hosts:      []string{"hass.willwhite.dev"},
			rootDomain: "willwhite.dev",
			ip:         "10.0.0.1",
			wantRules: []string{
				ManagedBlockStart,
				"||*.willwhite.dev^$dnsrewrite=NOERROR;A;10.0.0.1,denyallow=hass.willwhite.dev",
				ManagedBlockEnd,
			},
		},
		{
			name:       "Mixed Hosts",
			hosts:      []string{"hass.willwhite.dev", "plex.com"}, // plex.com should be ignored
			rootDomain: "willwhite.dev",
			ip:         "192.168.1.1",
			wantRules: []string{
				ManagedBlockStart,
				"||*.willwhite.dev^$dnsrewrite=NOERROR;A;192.168.1.1,denyallow=hass.willwhite.dev",
				ManagedBlockEnd,
			},
		},
		{
			name:       "No Hosts",
			hosts:      []string{},
			rootDomain: "willwhite.dev",
			ip:         "10.0.0.1",
			wantRules: []string{
				ManagedBlockStart,
				"||*.willwhite.dev^$dnsrewrite=NOERROR;A;10.0.0.1",
				ManagedBlockEnd,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := generateSmartRule(tt.hosts, tt.rootDomain, tt.ip)
			if !reflect.DeepEqual(got, tt.wantRules) {
				t.Errorf("generateSmartRule() = %v, want %v", got, tt.wantRules)
			}
		})
	}
}

func TestApplySmartRule(t *testing.T) {
	root := "domain.tld"
	newBlock := []string{
		ManagedBlockStart,
		"||*.domain.tld^$dnsrewrite=NOERROR;A;...,denyallow=host1.domain.tld",
		ManagedBlockEnd,
	}

	tests := []struct {
		name         string
		currentRules []string
		newBlock     []string
		rootDomain   string
		want         []string
		wantChanged  bool
	}{
		{
			name:         "Fresh start with cleanup",
			currentRules: []string{"||other.com^", "||*.domain.tld^NoRewite"},
			newBlock:     newBlock,
			rootDomain:   root,
			want:         append([]string{"||other.com^"}, newBlock...),
			wantChanged:  true,
		},
		{
			name:         "Already exists identical",
			currentRules: append([]string{"||other.com^"}, newBlock...),
			newBlock:     newBlock,
			rootDomain:   root,
			want:         append([]string{"||other.com^"}, newBlock...),
			wantChanged:  false,
		},
		{
			name:         "Update existing block",
			currentRules: []string{ManagedBlockStart, "OldContent", ManagedBlockEnd},
			newBlock:     newBlock,
			rootDomain:   root,
			want:         newBlock,
			wantChanged:  true,
		},
		{
			name:         "Migrate legacy regex",
			currentRules: []string{"||other.com^", "/^(?!host$).+\\.domain\\.tld$/$dnsrewrite..."},
			newBlock:     newBlock,
			rootDomain:   root,
			want:         append([]string{"||other.com^"}, newBlock...),
			wantChanged:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, changed := applySmartRule(tt.currentRules, tt.newBlock, tt.rootDomain)
			if changed != tt.wantChanged {
				t.Errorf("applySmartRule() changed = %v, want %v", changed, tt.wantChanged)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("applySmartRule() got = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestUpdateUserRules_ContentTypeHeader(t *testing.T) {
	var receivedContentType string
	var receivedBody []byte

	// Create a test server that captures the Content-Type header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedContentType = r.Header.Get("Content-Type")
		receivedBody, _ = io.ReadAll(r.Body)

		if r.URL.Path == "/control/filtering/set_rules" {
			if receivedContentType != "application/json" {
				w.WriteHeader(415)
				w.Write([]byte("only content-type application/json is allowed"))
				return
			}
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	config := Config{
		AdGuardURL:    server.URL,
		AdGuardUser:   "testuser",
		AdGuardPass:   "testpass",
		RootDomain:    "willwhite.dev",
		FallbackIP:    "1.2.3.4",
		CheckInterval: 60 * time.Second,
		HealthPort:    "8080",
	}

	rules := []string{"rule1", "rule2", "rule3"}
	err := updateUserRules(config, rules)

	if err != nil {
		t.Errorf("updateUserRules() returned error: %v", err)
	}

	if receivedContentType != "application/json" {
		t.Errorf("Content-Type header = '%s', want 'application/json'", receivedContentType)
	}

	// Verify the body is valid JSON with the expected structure
	var payload struct {
		Rules []string `json:"rules"`
	}
	if err := json.Unmarshal(receivedBody, &payload); err != nil {
		t.Errorf("Failed to unmarshal request body: %v", err)
	}

	if !reflect.DeepEqual(payload.Rules, rules) {
		t.Errorf("Request body rules = %v, want %v", payload.Rules, rules)
	}

	t.Logf("✓ Content-Type header correctly set to: %s", receivedContentType)
	t.Logf("✓ Request body: %s", string(receivedBody))
}

func TestLoadConfigDefaults(t *testing.T) {
	t.Setenv("ADGUARD_URL", "http://adguard.test:3000/") // trailing slash must be trimmed
	t.Setenv("ADGUARD_USER", "admin")
	t.Setenv("ADGUARD_PASS", "secret")
	t.Setenv("ROOT_DOMAIN", ".domain.tld") // leading dot must be stripped
	t.Setenv("FALLBACK_IP", "192.0.2.1")
	t.Setenv("CHECK_INTERVAL", "")
	t.Setenv("HEALTH_PORT", "")

	got := loadConfig()

	if got.AdGuardURL != "http://adguard.test:3000" {
		t.Errorf("AdGuardURL = %q, want trailing slash trimmed", got.AdGuardURL)
	}
	if got.RootDomain != "domain.tld" {
		t.Errorf("RootDomain = %q, want leading dot stripped", got.RootDomain)
	}
	if got.CheckInterval != 60*time.Second {
		t.Errorf("CheckInterval = %v, want the 60s default", got.CheckInterval)
	}
	if got.HealthPort != "8080" {
		t.Errorf("HealthPort = %q, want the 8080 default", got.HealthPort)
	}
}

func TestLoadConfigOverrides(t *testing.T) {
	t.Setenv("ADGUARD_URL", "https://adguard.test")
	t.Setenv("ADGUARD_USER", "admin")
	t.Setenv("ADGUARD_PASS", "secret")
	t.Setenv("ROOT_DOMAIN", "domain.tld")
	t.Setenv("FALLBACK_IP", "192.0.2.1")
	t.Setenv("CHECK_INTERVAL", "15")
	t.Setenv("HEALTH_PORT", "9090")

	got := loadConfig()

	if got.CheckInterval != 15*time.Second {
		t.Errorf("CheckInterval = %v, want 15s", got.CheckInterval)
	}
	if got.HealthPort != "9090" {
		t.Errorf("HealthPort = %q, want 9090", got.HealthPort)
	}
}

func TestHealthHandlers(t *testing.T) {
	origHealthy, origLastCheck := healthy.Load(), lastCheckOK.Load()
	t.Cleanup(func() {
		healthy.Store(origHealthy)
		lastCheckOK.Store(origLastCheck)
	})

	tests := []struct {
		name        string
		healthy     bool
		lastCheckOK bool
		wantCode    int
		wantBody    string
	}{
		{"serving and last sync ok", true, true, http.StatusOK, "OK"},
		{"last sync failed", true, false, http.StatusServiceUnavailable, "UNHEALTHY"},
		{"health server broken", false, true, http.StatusServiceUnavailable, "UNHEALTHY"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			healthy.Store(tt.healthy)
			lastCheckOK.Store(tt.lastCheckOK)

			rec := httptest.NewRecorder()
			handleHealthz(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

			if rec.Code != tt.wantCode {
				t.Errorf("/healthz status = %d, want %d", rec.Code, tt.wantCode)
			}
			if rec.Body.String() != tt.wantBody {
				t.Errorf("/healthz body = %q, want %q", rec.Body.String(), tt.wantBody)
			}
		})
	}

	// /readyz must stay 200 even when the sidecar is unhealthy: it is the
	// liveness signal, and AdGuard being down must not restart the process.
	healthy.Store(false)
	lastCheckOK.Store(false)
	rec := httptest.NewRecorder()
	handleReadyz(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "READY" {
		t.Errorf("/readyz = %d %q while unhealthy, want 200 \"READY\"", rec.Code, rec.Body.String())
	}
}

func TestFetchUserRules(t *testing.T) {
	var gotPath, gotUser, gotPass string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotUser, gotPass, _ = r.BasicAuth()
		json.NewEncoder(w).Encode(FilteringStatus{UserRules: []string{"||a^", "||b^"}})
	}))
	defer server.Close()

	config := Config{AdGuardURL: server.URL, AdGuardUser: "admin", AdGuardPass: "secret"}
	rules, err := fetchUserRules(config)
	if err != nil {
		t.Fatalf("fetchUserRules() error = %v", err)
	}

	if want := []string{"||a^", "||b^"}; !reflect.DeepEqual(rules, want) {
		t.Errorf("rules = %v, want %v", rules, want)
	}
	if gotPath != "/control/filtering/status" {
		t.Errorf("path = %q, want /control/filtering/status", gotPath)
	}
	if gotUser != "admin" || gotPass != "secret" {
		t.Errorf("basic auth = %q/%q, want admin/secret", gotUser, gotPass)
	}
}

func TestFetchUserRulesError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte("forbidden"))
	}))
	defer server.Close()

	_, err := fetchUserRules(Config{AdGuardURL: server.URL})
	if err == nil {
		t.Fatal("fetchUserRules() error = nil, want an error on 403")
	}
}

// adguardStub emulates the two endpoints syncRules uses, tracking how many
// times the rules were actually written.
type adguardStub struct {
	mu     sync.Mutex
	rules  []string
	writes int
}

func (a *adguardStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()

		switch r.URL.Path {
		case "/control/filtering/status":
			json.NewEncoder(w).Encode(FilteringStatus{UserRules: a.rules})
		case "/control/filtering/set_rules":
			var payload struct {
				Rules []string `json:"rules"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			a.rules = payload.Rules
			a.writes++
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func (a *adguardStub) snapshot() ([]string, int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.rules...), a.writes
}

func TestSyncRulesWritesOnceThenNoOps(t *testing.T) {
	stub := &adguardStub{rules: []string{"||keep.me^"}}
	server := httptest.NewServer(stub.handler())
	defer server.Close()

	config := Config{
		AdGuardURL: server.URL,
		RootDomain: "domain.tld",
		FallbackIP: "192.0.2.1",
	}
	k8s := &stubK8sClient{hosts: []string{"hass.domain.tld", "elsewhere.example.com"}}

	if err := syncRules(config, k8s); err != nil {
		t.Fatalf("first syncRules() error = %v", err)
	}

	want := []string{
		"||keep.me^",
		ManagedBlockStart,
		"||*.domain.tld^$dnsrewrite=NOERROR;A;192.0.2.1,denyallow=hass.domain.tld",
		ManagedBlockEnd,
	}
	got, writes := stub.snapshot()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rules after sync = %v, want %v", got, want)
	}
	if writes != 1 {
		t.Errorf("writes = %d, want 1", writes)
	}

	// Nothing changed, so the second sync must not write again.
	if err := syncRules(config, k8s); err != nil {
		t.Fatalf("second syncRules() error = %v", err)
	}
	if _, writes = stub.snapshot(); writes != 1 {
		t.Errorf("writes after unchanged sync = %d, want still 1", writes)
	}
}

func TestSyncRulesIngressErrorDoesNotWrite(t *testing.T) {
	stub := &adguardStub{rules: []string{"||keep.me^"}}
	server := httptest.NewServer(stub.handler())
	defer server.Close()

	config := Config{AdGuardURL: server.URL, RootDomain: "domain.tld", FallbackIP: "192.0.2.1"}
	k8s := &stubK8sClient{err: io.ErrUnexpectedEOF}

	if err := syncRules(config, k8s); err == nil {
		t.Fatal("syncRules() error = nil, want the ingress failure surfaced")
	}
	if _, writes := stub.snapshot(); writes != 0 {
		t.Errorf("writes = %d, want 0 when the ingress list fails", writes)
	}
}

func TestApplySmartRuleMovesBlockBelowLaterRules(t *testing.T) {
	newBlock := []string{ManagedBlockStart, "||*.domain.tld^$dnsrewrite=NOERROR;A;192.0.2.1", ManagedBlockEnd}
	current := append(append([]string{"||before^"}, newBlock...), "||after^")

	got, changed := applySmartRule(current, newBlock, "domain.tld")

	want := []string{"||before^", "||after^", ManagedBlockStart, "||*.domain.tld^$dnsrewrite=NOERROR;A;192.0.2.1", ManagedBlockEnd}
	if !changed {
		t.Error("changed = false, want true: the block has to move below ||after^")
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got = %v, want %v", got, want)
	}
}

func TestGenerateSmartRuleIgnoresApexHost(t *testing.T) {
	// The wildcard ||*.domain.tld^ never matches the apex, so exempting it
	// would be meaningless.
	got := generateSmartRule([]string{"domain.tld", "app.domain.tld"}, "domain.tld", "192.0.2.1")

	want := []string{
		ManagedBlockStart,
		"||*.domain.tld^$dnsrewrite=NOERROR;A;192.0.2.1,denyallow=app.domain.tld",
		ManagedBlockEnd,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got = %v, want %v", got, want)
	}
}
