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
	ManagedBlockStart = "! -- ADGUARD EXTERNAL DNS SIDECAR START --"
	ManagedBlockEnd   = "! -- ADGUARD EXTERNAL DNS SIDECAR END --"
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

	// Fetch current rules
	rules, err := fetchUserRules(config)
	if err != nil {
		return fmt.Errorf("failed to fetch rules: %w", err)
	}

	// Process rules
	newRules := generateSmartRule(hosts, config.RootDomain, config.FallbackIP)
	updatedRules, needsUpdate := applySmartRule(rules, newRules, config.RootDomain)

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

// generateSmartRule creates the list of rules to be applied
func generateSmartRule(hosts []string, rootDomain, ip string) []string {
	var rules []string

	rules = append(rules, ManagedBlockStart)

	// We use the $denyallow modifier to exempt known hosts from the catch-all rewrite.
	// This allows other specific rules (like those from ExternalDNS) to take precedence
	// or for the query to pass through if no other rule exists.
	// Syntax: ||*.root^$dnsrewrite=...,denyallow=host1|host2

	var deniedHosts []string
	for _, h := range hosts {
		if strings.HasSuffix(h, "."+rootDomain) {
			deniedHosts = append(deniedHosts, h)
		}
	}

	wildcardRule := fmt.Sprintf("||*.%s^$dnsrewrite=NOERROR;A;%s", rootDomain, ip)

	if len(deniedHosts) > 0 {
		denyList := strings.Join(deniedHosts, "|")
		wildcardRule = fmt.Sprintf("%s,denyallow=%s", wildcardRule, denyList)
	}

	rules = append(rules, wildcardRule)
	rules = append(rules, ManagedBlockEnd)
	return rules
}

// applySmartRule update the rules list with the new managed block
func applySmartRule(currentRules []string, newManagedBlock []string, rootDomain string) ([]string, bool) {
	var preBlock []string
	var inManagedBlock bool
	var foundManagedBlock bool

	// Scan current rules to split into pre-block and post-block
	// and identify if we have an existing managed block
	for _, r := range currentRules {
		if r == ManagedBlockStart {
			inManagedBlock = true
			foundManagedBlock = true
			continue
		}
		if r == ManagedBlockEnd {
			inManagedBlock = false
			continue
		}

		if inManagedBlock {
			// We skip the old managed content
			continue
		}

		// Cleanup legacy rules if we didn't find a proper block yet
		// (This handles migration from v0.1/v0.2 logic)
		if !foundManagedBlock {
			// 1. Old static wildcard: ||*.rootDomain^...
			oldWildcardStart := fmt.Sprintf("||*.%s^", rootDomain)
			if strings.HasPrefix(r, oldWildcardStart) {
				continue
			}
			// 2. Old regex rules
			escapedRoot := regexp.QuoteMeta(rootDomain)
			if strings.HasPrefix(r, "/") && strings.Contains(r, escapedRoot) && strings.Contains(r, "$dnsrewrite") {
				continue
			}
		}

		// Keep user rule
		preBlock = append(preBlock, r)
	}

	// If we are appending to the end (no post-block logic needed really, effectively we wiped the old block)
	// But wait, if user had rules AFTER our block?
	// The loop above puts everything NOT in the block into `preBlock`.
	// This creates a side effect: our block always moves to the end.
	// This is generally fine or even desired ensuring priority?
	// Actually, careful: In AdGuard, order matters.
	// Exceptions (@@) generally override regardless of position, but wildcards vs allowlists...
	// If we simply rebuild the list as [UserRules... NewBlock...], that is safe.

	// Construct candidate new list
	result := append(preBlock, newManagedBlock...)

	// Compare with currentRules to see if anything changed
	if len(result) != len(currentRules) {
		return result, true
	}
	for i, v := range result {
		if v != currentRules[i] {
			return result, true
		}
	}

	return result, false
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
