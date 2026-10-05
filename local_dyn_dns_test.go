package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

const validConfig = `database:
  host: "db.example"
  port: 5432
  name: "monitoring"
  user: "monitoring"
  password: "secret"
technitium:
  base_url: "http://127.0.0.1:5380/"
  api_token: "token"
  zone: "Example.LAN."
  ttl_seconds: 60
settings:
  dns_concurrency: 8
  dns_timeout_seconds: 2.5
`

func parseYAML(t *testing.T, text string) (Config, error) {
	t.Helper()
	var document any
	if err := yaml.Unmarshal([]byte(text), &document); err != nil {
		t.Fatal(err)
	}
	return parseConfig(document)
}

func TestParseConfig(t *testing.T) {
	config, err := parseYAML(t, validConfig)
	if err != nil {
		t.Fatal(err)
	}
	if config.Technitium.BaseURL != "http://127.0.0.1:5380" || config.Technitium.Zone != "example.lan" {
		t.Fatalf("unexpected technitium config: %+v", config.Technitium)
	}
	if config.Settings.Timeout != 2500*time.Millisecond {
		t.Fatalf("unexpected timeout: %v", config.Settings.Timeout)
	}

	cases := map[string][2]string{
		"placeholder":     {`api_token: "token"`, `api_token: "change_me"`},
		"unknown key":     {`settings:`, "settings:\n  retries: 3"},
		"url credentials": {`http://127.0.0.1`, `http://user:pw@127.0.0.1`},
		"negative ttl":    {`ttl_seconds: 60`, `ttl_seconds: -1`},
		"zero timeout":    {`dns_timeout_seconds: 2.5`, `dns_timeout_seconds: 0`},
		"too many backups": {`password: "secret"`,
			`password: "secret"` + "\n  backups: [{}, {}, {}, {}]"},
	}
	for name, replacement := range cases {
		if _, err := parseYAML(t, strings.Replace(validConfig, replacement[0], replacement[1], 1)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestFirstRunCreatesPrivateTemplate(t *testing.T) {
	binaryDir, home := t.TempDir(), t.TempDir()
	_, _, err := loadConfig(binaryDir, home)
	if _, ok := err.(*configCreatedError); !ok {
		t.Fatalf("unexpected error: %v", err)
	}
	info, err := os.Stat(filepath.Join(home, ".config", projectName, configFilename))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("unexpected template: %v %v", info, err)
	}
}

func TestNormalizeDNSName(t *testing.T) {
	for input, want := range map[string]string{
		" Host.Example.LAN. ": "host.example.lan",
		"bücher.lan":          "xn--bcher-kva.lan",
	} {
		if got, err := normalizeDNSName(input); err != nil || got != want {
			t.Errorf("%q: got %q, %v", input, got, err)
		}
	}
	for _, input := range []string{"", "*.example.lan", "http://example.lan", "bad_label.lan", "-a.lan"} {
		if _, err := normalizeDNSName(input); err == nil {
			t.Errorf("%q: expected an error", input)
		}
	}
}

func TestSelectRecords(t *testing.T) {
	selection := selectRecords([]HostRow{
		{true, "192.0.2.1", "single.lan", nil},                            // unique: priority ignored
		{"TRUE", netip.MustParsePrefix("2001:db8::1/128"), "v6.lan", nil}, // inet value
		{true, "192.0.2.2", "multi.lan", "1"},                             // loses by priority
		{true, "192.0.2.3", "multi.lan", "10.5"},                          // wins by priority
		{true, "192.0.2.4", "tie.lan", "5"},                               // ambiguous tie
		{true, "192.0.2.5", "tie.lan", "5.0"},                             // ambiguous tie
		{true, "192.0.2.6", "Case.lan", nil},                              // variants disagree
		{true, "192.0.2.7", "case.lan", nil},                              // variants disagree
		{true, "192.0.2.8", "nopriority.lan", nil},                        // repeated without priority
		{true, "192.0.2.9", "nopriority.lan", "NaN"},                      // repeated, not finite
		{false, "garbage", "ignored", nil},                                // inactive
		{nil, nil, nil, nil},                                              // NULL is inactive
		{"maybe", "192.0.2.10", "status.lan", nil},                        // invalid status
		{true, "not-an-ip", "ip.lan", nil},                                // invalid IP
		{true, "192.0.2.11", "*.wild.lan", nil},                           // invalid name
	})

	want := []Candidate{
		{"multi.lan", "192.0.2.3", "A"},
		{"single.lan", "192.0.2.1", "A"},
		{"v6.lan", "2001:db8::1", "AAAA"},
	}
	if !reflect.DeepEqual(selection.Selected, want) {
		t.Fatalf("unexpected selection: %+v", selection.Selected)
	}
	if selection.RowsRead != 15 || selection.Inactive != 2 || len(selection.Invalid) != 5 ||
		selection.NonSelected != 5 || len(selection.Ambiguous) != 2 {
		t.Fatalf("unexpected totals: %+v", selection)
	}
}

// Fake Technitium zone holding records per name
func fakeTechnitium(t *testing.T, records map[string][]map[string]any) *TechnitiumClient {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer token" {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		domain := request.PostFormValue("domain")
		if strings.HasSuffix(request.URL.Path, "/add") {
			records[domain] = []map[string]any{{
				"name": domain, "type": request.PostFormValue("type"), "ttl": 60, "disabled": false,
				"rData": map[string]any{"ipAddress": request.PostFormValue("ipAddress")},
			}}
		}
		// Empty list instead of JSON null
		found := append([]map[string]any{}, records[domain]...)
		json.NewEncoder(writer).Encode(map[string]any{
			"status": "ok", "response": map[string]any{"records": found},
		})
	}))
	t.Cleanup(server.Close)
	return newTechnitiumClient(TechnitiumConfig{BaseURL: server.URL, APIToken: "token", Zone: "lan", TTLSeconds: 60})
}

func TestSynchronize(t *testing.T) {
	client := fakeTechnitium(t, map[string][]map[string]any{
		"old.lan":   {{"name": "old.lan", "type": "A", "ttl": 60, "rData": map[string]any{"ipAddress": "192.0.2.99"}}},
		"alias.lan": {{"name": "alias.lan", "type": "CNAME", "ttl": 60}},
	})
	candidates := []Candidate{
		{"new.lan", "192.0.2.1", "A"},
		{"old.lan", "192.0.2.2", "A"},
		{"alias.lan", "192.0.2.3", "A"},
		{"host.example.org", "192.0.2.4", "A"},
	}
	settings := Settings{Concurrency: 2, Timeout: 5 * time.Second}
	want := []string{stateCreated, stateUpdated, stateSkipped, stateSkipped}
	for index, result := range synchronizeAll(context.Background(), candidates, client, settings) {
		if result.State != want[index] {
			t.Errorf("%s: got %s (%s)", result.Candidate.Address, result.State, result.Detail)
		}
	}
	// Second run finds matching records
	for _, result := range synchronizeAll(context.Background(), candidates[:2], client, settings) {
		if result.State != stateUnchanged {
			t.Errorf("%s: got %s (%s)", result.Candidate.Address, result.State, result.Detail)
		}
	}

	client.config.APIToken = "wrong"
	if result := client.synchronize(context.Background(), candidates[0], time.Second); result.ErrorKind != "authentication" {
		t.Errorf("unexpected result: %+v", result)
	}
}
