package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

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
