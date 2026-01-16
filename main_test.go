package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestGenerateSmartRule(t *testing.T) {
	tests := []struct {
		name        string
		hosts       []string
		rootDomain  string
		ip          string
		wantPrefix  string
		wantSuffix  string
		wantContain string
	}{
		{
			name:        "Single Host",
			hosts:       []string{"hass.willwhite.dev"},
			rootDomain:  "willwhite.dev",
			ip:          "10.0.0.1",
			wantPrefix:  "/^(?!hass\\.willwhite\\.dev$).",
			wantSuffix:  "$dnsrewrite=NOERROR;A;10.0.0.1",
			wantContain: "willwhite\\.dev",
		},
		{
			name:        "Multiple Hosts",
			hosts:       []string{"hass.willwhite.dev", "plex.willwhite.dev"},
			rootDomain:  "willwhite.dev",
			ip:          "192.168.1.1",
			wantPrefix:  "/^(?!hass\\.willwhite\\.dev$|plex\\.willwhite\\.dev$)",
			wantSuffix:  "192.168.1.1",
			wantContain: "|",
		},
		{
			name:        "No Hosts",
			hosts:       []string{},
			rootDomain:  "willwhite.dev",
			ip:          "10.0.0.1",
			wantPrefix:  "||*.willwhite.dev^",
			wantSuffix:  "10.0.0.1",
			wantContain: "$dnsrewrite=NOERROR",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := generateSmartRule(tt.hosts, tt.rootDomain, tt.ip)
			if !strings.HasPrefix(got, tt.wantPrefix) {
				t.Errorf("generateSmartRule() prefix = %v, want prefix %v", got, tt.wantPrefix)
			}
			if !strings.HasSuffix(got, tt.wantSuffix) {
				t.Errorf("generateSmartRule() suffix = %v, want suffix %v", got, tt.wantSuffix)
			}
			if tt.wantContain != "" && !strings.Contains(got, tt.wantContain) {
				t.Errorf("generateSmartRule() does not contain %v", tt.wantContain)
			}
		})
	}
}

func TestApplySmartRule(t *testing.T) {
	root := "domain.tld"
	newRule := "/^(?!host1$).+\\.domain\\.tld$/$dnsrewrite=..."

	tests := []struct {
		name         string
		currentRules []string
		newRule      string
		rootDomain   string
		want         []string
		wantChanged  bool
	}{
		{
			name:         "Fresh start",
			currentRules: []string{"||other.com^"},
			newRule:      newRule,
			rootDomain:   root,
			want:         []string{"||other.com^", newRule},
			wantChanged:  true,
		},
		{
			name:         "Already exists at bottom",
			currentRules: []string{"||other.com^", newRule},
			newRule:      newRule,
			rootDomain:   root,
			want:         []string{"||other.com^", newRule},
			wantChanged:  false,
		},
		{
			name:         "Already exists but wrong position",
			currentRules: []string{newRule, "||other.com^"},
			newRule:      newRule,
			rootDomain:   root,
			want:         []string{"||other.com^", newRule},
			wantChanged:  true,
		},
		{
			name:         "Remove old wildcard",
			currentRules: []string{"||*.domain.tld^$dnsrewrite...", "||other.com^"},
			newRule:      newRule,
			rootDomain:   root,
			want:         []string{"||other.com^", newRule},
			wantChanged:  true,
		},
		{
			name:         "Update existing smart rule",
			currentRules: []string{"||other.com^", "/^(?!old$).+\\.domain\\.tld$/$dnsrewrite=NOERROR..."},
			newRule:      newRule,
			rootDomain:   root,
			want:         []string{"||other.com^", newRule},
			wantChanged:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, changed := applySmartRule(tt.currentRules, tt.newRule, tt.rootDomain)
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
