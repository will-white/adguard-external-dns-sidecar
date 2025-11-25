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
	TargetRule    string
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
	log.Printf("Configuration loaded: URL=%s, Target Rule=%s, Check Interval=%v",
		config.AdGuardURL, config.TargetRule, config.CheckInterval)

	// Start health check server
	go startHealthServer(config.HealthPort)

	// Run the main loop
	ticker := time.NewTicker(config.CheckInterval)
	defer ticker.Stop()

	// Run immediately on startup
	if err := enforceRulePosition(config); err != nil {
		log.Printf("Error on initial check: %v", err)
		lastCheckOK = false
	} else {
		lastCheckOK = true
	}

	// Then run on interval
	for range ticker.C {
		if err := enforceRulePosition(config); err != nil {
			log.Printf("Error enforcing rule position: %v", err)
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
		TargetRule:  getEnvOrFatal("TARGET_RULE"),
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

	return config
}

func getEnvOrFatal(key string) string {
	value := os.Getenv(key)
	if value == "" {
		log.Fatalf("Required environment variable %s is not set", key)
	}
	return value
}

func enforceRulePosition(config Config) error {
	// Fetch current rules
	rules, err := fetchUserRules(config)
	if err != nil {
		return fmt.Errorf("failed to fetch rules: %w", err)
	}

	log.Printf("Fetched %d user rules from AdGuard", len(rules))

	// Check if target rule is at the bottom
	if isRuleAtBottom(rules, config.TargetRule) {
		log.Println("Target rule is already at the bottom. No action needed.")
		return nil
	}

	// Remove all occurrences of the target rule and append it to the end
	updatedRules := removeRule(rules, config.TargetRule)
	updatedRules = append(updatedRules, config.TargetRule)

	log.Printf("Moving target rule to bottom position (rule %d of %d)", len(updatedRules), len(updatedRules))

	// Update rules in AdGuard
	if err := updateUserRules(config, updatedRules); err != nil {
		return fmt.Errorf("failed to update rules: %w", err)
	}

	log.Println("Successfully updated user rules in AdGuard")
	return nil
}

func isRuleAtBottom(rules []string, targetRule string) bool {
	if len(rules) == 0 {
		return false
	}
	// Check if the last rule matches the target
	return rules[len(rules)-1] == targetRule
}

func removeRule(rules []string, targetRule string) []string {
	var result []string
	for _, rule := range rules {
		if rule != targetRule {
			result = append(result, rule)
		}
	}
	return result
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
