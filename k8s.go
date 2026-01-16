package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
)

// K8sClient interface for fetching hosts, allowing for mocking in tests
type K8sClient interface {
	GetIngressHosts() ([]string, error)
}

// ClusterClient implements K8sClient for in-cluster usage
type ClusterClient struct {
	apiServer string
	token     string
	caCert    []byte
	client    *http.Client
}

// IngressList is a minimal struct to parse Ingress resources
type IngressList struct {
	Items []struct {
		Spec struct {
			Rules []struct {
				Host string `json:"host"`
			} `json:"rules"`
		} `json:"spec"`
	} `json:"items"`
}

func NewK8sClient() (K8sClient, error) {
	host := os.Getenv("KUBERNETES_SERVICE_HOST")
	port := os.Getenv("KUBERNETES_SERVICE_PORT")

	// Fallback/Dev mode if not in cluster
	if host == "" || port == "" {
		log.Println("KUBERNETES_SERVICE_HOST not set, using MockClient (returning empty list)")
		return &MockClient{}, nil
	}

	tokenData, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/token")
	if err != nil {
		return nil, fmt.Errorf("failed to read service account token: %v", err)
	}
	token := string(tokenData)

	caCert, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/ca.crt")
	if err != nil {
		return nil, fmt.Errorf("failed to read service account CA: %v", err)
	}

	caCertPool := x509.NewCertPool()
	caCertPool.AppendCertsFromPEM(caCert)

	tlsConfig := &tls.Config{
		RootCAs: caCertPool,
	}

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: tlsConfig,
		},
	}

	return &ClusterClient{
		apiServer: "https://" + host + ":" + port,
		token:     token,
		caCert:    caCert,
		client:    client,
	}, nil
}

func (c *ClusterClient) GetIngressHosts() ([]string, error) {
	url := c.apiServer + "/apis/networking.k8s.io/v1/ingresses"
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API server returned %d: %s", resp.StatusCode, string(body))
	}

	var ingressList IngressList
	if err := json.NewDecoder(resp.Body).Decode(&ingressList); err != nil {
		return nil, err
	}

	var hosts []string
	seen := make(map[string]bool)

	for _, item := range ingressList.Items {
		for _, rule := range item.Spec.Rules {
			if rule.Host != "" && !seen[rule.Host] {
				hosts = append(hosts, rule.Host)
				seen[rule.Host] = true
			}
		}
	}

	return hosts, nil
}

// MockClient for local development or when not running in K8s
type MockClient struct{}

func (m *MockClient) GetIngressHosts() ([]string, error) {
	// Return empty list or basic defaults for testing
	return []string{}, nil
}
