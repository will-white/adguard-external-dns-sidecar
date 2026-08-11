package main

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestNewK8sClientFallsBackToMock(t *testing.T) {
	// Outside a cluster the sidecar must degrade to the mock rather than fail,
	// so `docker compose up` and local runs still work.
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")

	client, err := NewK8sClient()
	if err != nil {
		t.Fatalf("NewK8sClient() error = %v", err)
	}
	if _, ok := client.(*MockClient); !ok {
		t.Fatalf("NewK8sClient() = %T, want *MockClient", client)
	}

	hosts, err := client.GetIngressHosts()
	if err != nil {
		t.Fatalf("GetIngressHosts() error = %v", err)
	}
	if len(hosts) != 0 {
		t.Errorf("hosts = %v, want empty", hosts)
	}
}

func TestClusterClientGetIngressHosts(t *testing.T) {
	const body = `{"items":[
		{"spec":{"rules":[{"host":"a.domain.tld"},{"host":"b.domain.tld"}]}},
		{"spec":{"rules":[{"host":"a.domain.tld"},{"host":""}]}},
		{"spec":{"rules":[{"host":"c.domain.tld"}]}}
	]}`

	var gotPath, gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Write([]byte(body))
	}))
	defer server.Close()

	client := &ClusterClient{apiServer: server.URL, token: "tok", client: server.Client()}

	hosts, err := client.GetIngressHosts()
	if err != nil {
		t.Fatalf("GetIngressHosts() error = %v", err)
	}

	// Duplicates collapse, blank hosts are dropped, first-seen order is kept.
	want := []string{"a.domain.tld", "b.domain.tld", "c.domain.tld"}
	if !reflect.DeepEqual(hosts, want) {
		t.Errorf("hosts = %v, want %v", hosts, want)
	}
	if gotPath != "/apis/networking.k8s.io/v1/ingresses" {
		t.Errorf("path = %q, want the cluster-wide ingresses collection", gotPath)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("Authorization = %q, want \"Bearer tok\"", gotAuth)
	}
}

func TestClusterClientGetIngressHostsForbidden(t *testing.T) {
	// Missing RBAC is the most common deployment failure; it must surface as an
	// error rather than an empty host list, which would wipe every denyallow
	// exemption from the managed rule.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"message":"ingresses is forbidden"}`))
	}))
	defer server.Close()

	client := &ClusterClient{apiServer: server.URL, token: "tok", client: server.Client()}

	if _, err := client.GetIngressHosts(); err == nil {
		t.Fatal("GetIngressHosts() error = nil, want an error on 403")
	}
}
