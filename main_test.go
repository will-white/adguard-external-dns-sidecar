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

func TestIsRuleAtBottom(t *testing.T) {
	tests := []struct {
		name       string
		rules      []string
		targetRule string
		want       bool
	}{
		{
			name:       "Rule at bottom",
			rules:      []string{"rule1", "rule2", "target"},
			targetRule: "target",
			want:       true,
		},
		{
			name:       "Rule not at bottom",
			rules:      []string{"rule1", "target", "rule2"},
			targetRule: "target",
			want:       false,
		},
		{
			name:       "Rule missing",
			rules:      []string{"rule1", "rule2"},
			targetRule: "target",
			want:       false,
		},
		{
			name:       "Empty rules",
			rules:      []string{},
			targetRule: "target",
			want:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRuleAtBottom(tt.rules, tt.targetRule); got != tt.want {
				t.Errorf("isRuleAtBottom() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRemoveRule(t *testing.T) {
	tests := []struct {
		name       string
		rules      []string
		targetRule string
		want       []string
	}{
		{
			name:       "Remove existing rule",
			rules:      []string{"rule1", "target", "rule2"},
			targetRule: "target",
			want:       []string{"rule1", "rule2"},
		},
		{
			name:       "Remove multiple occurrences",
			rules:      []string{"target", "rule1", "target"},
			targetRule: "target",
			want:       []string{"rule1"},
		},
		{
			name:       "Rule not present",
			rules:      []string{"rule1", "rule2"},
			targetRule: "target",
			want:       []string{"rule1", "rule2"},
		},
		{
			name:       "Empty rules",
			rules:      []string{},
			targetRule: "target",
			want:       nil, // append to nil slice returns nil if nothing appended? No, it returns a slice. Wait.
			// In removeRule: var result []string. If loop doesn't append, it returns nil.
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := removeRule(tt.rules, tt.targetRule)
			// Handle nil vs empty slice comparison if necessary, but reflect.DeepEqual handles nil and empty slice differently.
			// removeRule returns nil if no elements are appended to result (which is initialized as nil).
			if len(got) == 0 && len(tt.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("removeRule() = %v, want %v", got, tt.want)
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
		TargetRule:    "testrule",
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
