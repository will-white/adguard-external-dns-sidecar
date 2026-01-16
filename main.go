package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Health check client mode flag
var healthCheck = flag.Bool("health", false, "Run health check client and exit")

type Config struct {
	AdGuardURL    string
	AdGuardUser   string
	AdGuardPass   string
	RootDomain    string
	FallbackIP    string
	CheckInterval time.Duration
	HealthPort    string
}

type FilteringStatus struct {
	UserRules []string `json:"user_rules"`
}

// Health status for the health check endpoint
var (
	healthy     = true
	lastCheckOK = true
)

func main() {
	flag.Parse()

	// Health check client mode for Docker HEALTHCHECK
	if *healthCheck {
		port := os.Getenv("HEALTH_PORT")
		if port == "" {
			port = "8080"
		}
		resp, err := http.Get("http://localhost:" + port + "/healthz")
		if err != nil {
			os.Exit(1)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			os.Exit(1)
		}
		os.Exit(0)
	}

	log.Println("Starting AdGuard External-DNS Sidecar...")

	config := loadConfig()
	log.Printf("Configuration loaded: URL=%s, Root=%s, IP=%s, Check Interval=%v",
		config.AdGuardURL, config.RootDomain, config.FallbackIP, config.CheckInterval)

	// Start health check server
	go startHealthServer(config.HealthPort)

	// Initialize K8s Client
	k8sClient, err := NewK8sClient() // Defined in k8s.go
	if err != nil {
		log.Fatalf("Failed to create K8s client: %v", err)
	}

	// Run the main loop
	ticker := time.NewTicker(config.CheckInterval)
	defer ticker.Stop()

	// Run immediately on startup
	if err := syncRules(config, k8sClient); err != nil {
		log.Printf("Error on initial check: %v", err)
		lastCheckOK = false
	} else {
		lastCheckOK = true
	}

	// Then run on interval
	for range ticker.C {
		if err := syncRules(config, k8sClient); err != nil {
			log.Printf("Error syncing rules: %v", err)
			lastCheckOK = false
		} else {
			lastCheckOK = true
		}
	}
}

func startHealthServer(port string) {
	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if healthy && lastCheckOK {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("OK"))
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte("UNHEALTHY"))
		}
	})

	http.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("READY"))
	})

	log.Printf("Health server listening on port %s", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Printf("Health server error: %v", err)
		healthy = false
	}
}

func loadConfig() Config {
	config := Config{
		AdGuardURL:  getEnvOrFatal("ADGUARD_URL"),
		AdGuardUser: getEnvOrFatal("ADGUARD_USER"),
		AdGuardPass: getEnvOrFatal("ADGUARD_PASS"),
		RootDomain:  getEnvOrFatal("ROOT_DOMAIN"),
		FallbackIP:  getEnvOrFatal("FALLBACK_IP"),
	}

	// Parse CHECK_INTERVAL with default
	intervalStr := os.Getenv("CHECK_INTERVAL")
	if intervalStr == "" {
		config.CheckInterval = 60 * time.Second
	} else {
		seconds, err := strconv.Atoi(intervalStr)
		if err != nil {
			log.Fatalf("CHECK_INTERVAL must be a valid integer (seconds): %v", err)
		}
		if seconds <= 0 {
			log.Fatal("CHECK_INTERVAL must be greater than 0")
		}
		config.CheckInterval = time.Duration(seconds) * time.Second
	}

	// Parse HEALTH_PORT with default
	config.HealthPort = os.Getenv("HEALTH_PORT")
	if config.HealthPort == "" {
		config.HealthPort = "8080"
	}

	// Ensure URL doesn't end with slash
	config.AdGuardURL = strings.TrimSuffix(config.AdGuardURL, "/")

	// Normalize root domain (remove leading dot)
	config.RootDomain = strings.TrimPrefix(config.RootDomain, ".")

	return config
}

func getEnvOrFatal(key string) string {
	value := os.Getenv(key)
	if value == "" {
		log.Fatalf("Required environment variable %s is not set", key)
	}
	return value
}

// syncRules handles the core logic: Fetch hosts -> Generate Rule -> Update AdGuard
func syncRules(config Config, k8sClient K8sClient) error {
	hosts, err := k8sClient.GetIngressHosts()
	if err != nil {
		return fmt.Errorf("failed to fetch ingress hosts: %w", err)
	}

	// Generate the dynamic rule
	newSmartRule := generateSmartRule(hosts, config.RootDomain, config.FallbackIP)

	// Fetch current rules
	rules, err := fetchUserRules(config)
	if err != nil {
		return fmt.Errorf("failed to fetch rules: %w", err)
	}

	// Process rules
	updatedRules, needsUpdate := applySmartRule(rules, newSmartRule, config.RootDomain)

	if !needsUpdate {
		return nil
	}

	log.Printf("Updating user rules in AdGuard (Total: %d)", len(updatedRules))

	// Update rules in AdGuard
	if err := updateUserRules(config, updatedRules); err != nil {
		return fmt.Errorf("failed to update rules: %w", err)
	}

	log.Println("Successfully updated user rules in AdGuard")
	return nil
}

// generateSmartRule creates the negative lookahead regex
func generateSmartRule(hosts []string, rootDomain, ip string) string {
	// If no hosts, default to catch-all for the domain
	if len(hosts) == 0 {
		return fmt.Sprintf("||*.%s^$dnsrewrite=NOERROR;A;%s", rootDomain, ip)
	}

	var escapedHosts []string
	for _, h := range hosts {
		// We care about full FQDNs.
		// Regex: (?!host1\.domain\.com$|host2\.domain\.com$)
		escaped := regexp.QuoteMeta(h)
		escapedHosts = append(escapedHosts, escaped+"$")
	}

	// Join with pipe
	exclusionGroup := strings.Join(escapedHosts, "|")

	// Final Regex
	// /^(?!host1$|host2$).+\.willwhite\.dev$/
	// Note: We need to escape the root domain too for the matching part
	escapedRoot := regexp.QuoteMeta(rootDomain)

	// AdGuard Regex: /regex/$options
	return fmt.Sprintf("/^(?!%s).+\\.%s$/$dnsrewrite=NOERROR;A;%s", exclusionGroup, escapedRoot, ip)
}

// applySmartRule filters out old/conflicting rules and appends the new one
func applySmartRule(currentRules []string, newRule string, rootDomain string) ([]string, bool) {
	var result []string
	var changed bool

	// Definitions of what to remove:
	// 1. Old static wildcard: ||*.rootDomain^...
	// 2. regex rules targeting rootDomain

	oldWildcardStart := fmt.Sprintf("||*.%s^", rootDomain)

	foundExactMatch := false

	for _, r := range currentRules {
		// If it matches existing exact rule, we skip adding it here (will append later)
		if r == newRule {
			foundExactMatch = true
			continue
		}

		// Check if it is a rule we should manage (delete)
		if strings.HasPrefix(r, oldWildcardStart) {
			changed = true
			continue // Drop it
		}

		// Basic check for managed regex rule for this domain
		// This is a heuristic. We assume any regex rule matching our root domain is ours.
		// We check for the escaped root domain because regex rules will have it escaped.
		escapedRoot := regexp.QuoteMeta(rootDomain)
		if strings.HasPrefix(r, "/") && strings.Contains(r, escapedRoot) && strings.Contains(r, "$dnsrewrite") {
			changed = true
			continue
		}

		result = append(result, r)
	}

	// Always append the new rule at the bottom
	result = append(result, newRule)

	// Determine if update is needed
	if !foundExactMatch {
		changed = true
	} else {
		// Check order and content match
		if len(result) != len(currentRules) {
			changed = true
		} else {
			for i, v := range result {
				if v != currentRules[i] {
					changed = true
					break
				}
			}
		}
	}

	return result, changed
}

func fetchUserRules(config Config) ([]string, error) {
	url := fmt.Sprintf("%s/control/filtering/status", config.AdGuardURL)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}

	req.SetBasicAuth(config.AdGuardUser, config.AdGuardPass)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("unexpected status code %d: %s", resp.StatusCode, string(body))
	}

	var status FilteringStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return nil, err
	}

	return status.UserRules, nil
}

func updateUserRules(config Config, rules []string) error {
	url := fmt.Sprintf("%s/control/filtering/set_rules", config.AdGuardURL)

	// The API expects JSON with the rules array
	payload := struct {
		Rules []string `json:"rules"`
	}{
		Rules: rules,
	}

	jsonBody, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal rules: %w", err)
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonBody))
	if err != nil {
		return err
	}

	req.SetBasicAuth(config.AdGuardUser, config.AdGuardPass)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("unexpected status code %d: %s", resp.StatusCode, string(body))
	}

	return nil
}
