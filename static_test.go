package main

import (
	"net/http/httptest"
	"reflect"
	"testing"
)

func mustRecords(t *testing.T, raw string) []StaticRecord {
	t.Helper()
	r, err := parseStaticRecords(raw, "domain.tld")
	if err != nil {
		t.Fatalf("parseStaticRecords(%q) error = %v", raw, err)
	}
	return r
}

func TestParseStaticRecords(t *testing.T) {
	got := mustRecords(t, " nas=192.0.2.1, adguard.domain.tld=192.0.2.2 ,,v6=2001:db8::1,Upper.=192.0.2.3")
	var rules []string
	for _, r := range got {
		rules = append(rules, r.Rule())
	}
	want := []string{
		"|nas.domain.tld^$dnsrewrite=NOERROR;A;192.0.2.1,important",
		"|adguard.domain.tld^$dnsrewrite=NOERROR;A;192.0.2.2,important",
		"|v6.domain.tld^$dnsrewrite=NOERROR;AAAA;2001:db8::1,important",
		"|upper.domain.tld^$dnsrewrite=NOERROR;A;192.0.2.3,important",
	}
	if !reflect.DeepEqual(rules, want) {
		t.Errorf("rules = %v, want %v", rules, want)
	}
	if r := mustRecords(t, ""); len(r) != 0 {
		t.Errorf("empty input gave %v, want none", r)
	}
}

func TestParseStaticRecordsRejects(t *testing.T) {
	for _, raw := range []string{
		"nas",                         // no address
		"=192.0.2.1",                  // no name
		"nas=not-an-ip",               // bad address
		"evil.example.com=192.0.2.1",  // outside the root domain
		"a|b=192.0.2.1",               // rule syntax smuggled into the name
		"nas=192.0.2.1,nas=192.0.2.9", // duplicate A record
	} {
		if _, err := parseStaticRecords(raw, "domain.tld"); err == nil {
			t.Errorf("parseStaticRecords(%q) error = nil, want rejection", raw)
		}
	}
}

func TestGenerateRulesStaticOnly(t *testing.T) {
	config := Config{RootDomain: "domain.tld", NoWildcard: true, StaticRecords: mustRecords(t, "nas=192.0.2.1")}
	got := generateRules([]string{"hass.domain.tld"}, config)
	want := []string{
		ManagedBlockStart,
		"|nas.domain.tld^$dnsrewrite=NOERROR;A;192.0.2.1,important",
		ManagedBlockEnd,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got = %v, want %v", got, want)
	}
}

func TestGenerateRulesStaticPlusWildcardExemptsStatics(t *testing.T) {
	// A static host must be on the denyallow list, or AdGuard answers with
	// the union of its record and the wildcard's fallback address.
	config := Config{RootDomain: "domain.tld", FallbackIP: "192.0.2.254",
		StaticRecords: mustRecords(t, "adguard=192.0.2.2,hass=192.0.2.3")}
	got := generateRules([]string{"hass.domain.tld"}, config)
	want := []string{
		ManagedBlockStart,
		"|adguard.domain.tld^$dnsrewrite=NOERROR;A;192.0.2.2,important",
		"|hass.domain.tld^$dnsrewrite=NOERROR;A;192.0.2.3,important",
		"||*.domain.tld^$dnsrewrite=NOERROR;A;192.0.2.254,denyallow=hass.domain.tld|adguard.domain.tld",
		ManagedBlockEnd,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got = %v, want %v", got, want)
	}
}

func TestLoadConfigStaticModeNeedsNoFallback(t *testing.T) {
	t.Setenv("ADGUARD_URL", "http://adguard.test")
	t.Setenv("ADGUARD_USER", "admin")
	t.Setenv("ADGUARD_PASS", "secret")
	t.Setenv("ROOT_DOMAIN", ".domain.tld")
	t.Setenv("FALLBACK_IP", "")
	t.Setenv("WILDCARD_FALLBACK", "false")
	t.Setenv("STATIC_RECORDS", "nas=192.0.2.1")

	got := loadConfig()
	if !got.NoWildcard {
		t.Error("NoWildcard = false, want true")
	}
	if len(got.StaticRecords) != 1 || got.StaticRecords[0].Host != "nas.domain.tld" {
		t.Errorf("StaticRecords = %v, want nas.domain.tld (qualified with the dot-stripped root)", got.StaticRecords)
	}
}

func TestLoadConfigWildcardDefaultsOn(t *testing.T) {
	t.Setenv("ADGUARD_URL", "http://adguard.test")
	t.Setenv("ADGUARD_USER", "admin")
	t.Setenv("ADGUARD_PASS", "secret")
	t.Setenv("ROOT_DOMAIN", "domain.tld")
	t.Setenv("FALLBACK_IP", "192.0.2.1")
	t.Setenv("WILDCARD_FALLBACK", "")
	t.Setenv("STATIC_RECORDS", "")

	if got := loadConfig(); got.NoWildcard {
		t.Error("NoWildcard = true, want the default false so existing installs are unchanged")
	}
}

func TestSyncRulesStaticModeReplacesWildcardBlock(t *testing.T) {
	// An install switching modes: the old wildcard block is replaced in place
	// and user rules outside the block are kept.
	stub := &adguardStub{rules: []string{
		"||keep.me^",
		ManagedBlockStart,
		"||*.domain.tld^$dnsrewrite=NOERROR;A;192.0.2.254,denyallow=hass.domain.tld",
		ManagedBlockEnd,
	}}
	server := httptest.NewServer(stub.handler())
	defer server.Close()

	config := Config{AdGuardURL: server.URL, RootDomain: "domain.tld", NoWildcard: true, StaticRecords: mustRecords(t, "nas=192.0.2.1")}
	if err := syncRules(config, &stubK8sClient{hosts: []string{"hass.domain.tld"}}); err != nil {
		t.Fatalf("syncRules() error = %v", err)
	}
	want := []string{
		"||keep.me^",
		ManagedBlockStart,
		"|nas.domain.tld^$dnsrewrite=NOERROR;A;192.0.2.1,important",
		ManagedBlockEnd,
	}
	if got, _ := stub.snapshot(); !reflect.DeepEqual(got, want) {
		t.Errorf("rules = %v, want %v", got, want)
	}
}
