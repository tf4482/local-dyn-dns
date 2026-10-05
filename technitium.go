package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	stateCreated   = "created"
	stateUpdated   = "updated"
	stateUnchanged = "unchanged"
	stateSkipped   = "skipped"
	stateFailed    = "failed"
)

// Categorized API failure
type technitiumError struct {
	kind    string
	message string
}

func (e *technitiumError) Error() string { return e.message }

func apiError(kind, format string, args ...any) error {
	return &technitiumError{kind, fmt.Sprintf(format, args...)}
}

// Outcome for one DNS name
type SyncResult struct {
	Candidate Candidate
	State     string
	Detail    string
	ErrorKind string
}

// Technitium HTTP API client
type TechnitiumClient struct {
	config TechnitiumConfig
	http   *http.Client
}

func newTechnitiumClient(config TechnitiumConfig) *TechnitiumClient {
	// Never forward the token to a redirect target
	return &TechnitiumClient{config, &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// Synchronize all names with bounded concurrency; order is preserved
func synchronizeAll(ctx context.Context, candidates []Candidate, client *TechnitiumClient, settings Settings) []SyncResult {
	limit := make(chan struct{}, settings.Concurrency)
	results := make([]SyncResult, len(candidates))
	var group sync.WaitGroup
	for index, candidate := range candidates {
		group.Go(func() {
			limit <- struct{}{}
			defer func() { <-limit }()
			results[index] = client.synchronize(ctx, candidate, settings.Timeout)
		})
	}
	group.Wait()
	return results
}

// One name under a hard deadline covering all its requests
func (c *TechnitiumClient) synchronize(ctx context.Context, candidate Candidate, timeout time.Duration) SyncResult {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	mutated := false
	state, detail, err := c.reconcile(ctx, candidate, &mutated)
	var failure *technitiumError
	switch {
	case err == nil:
		return SyncResult{candidate, state, detail, ""}
	case errors.Is(err, context.DeadlineExceeded) && mutated:
		return SyncResult{candidate, stateFailed, "deadline exceeded after a mutation began; final DNS state is uncertain", "timeout"}
	case errors.Is(err, context.DeadlineExceeded):
		return SyncResult{candidate, stateFailed, "deadline exceeded before any mutation began", "timeout"}
	case errors.As(err, &failure) && failure.kind == "record-conflict":
		return SyncResult{candidate, stateSkipped, failure.message, failure.kind}
	case errors.As(err, &failure):
		return SyncResult{candidate, stateFailed, failure.message, failure.kind}
	}
	return SyncResult{candidate, stateFailed, "unexpected DNS client error", "api"}
}

// Create or overwrite the selected record set, then confirm it
func (c *TechnitiumClient) reconcile(ctx context.Context, candidate Candidate, mutated *bool) (string, string, error) {
	if !inZone(candidate.Address, c.config.Zone) {
		return "", "", apiError("record-conflict", "DNS name is outside configured zone %s", c.config.Zone)
	}
	records, err := c.records(ctx, candidate.Address)
	if err != nil {
		return "", "", err
	}
	if len(filterRecords(records, candidate.Address, "CNAME")) > 0 {
		return "", "", apiError("record-conflict", "an existing CNAME conflicts with the selected address record")
	}
	existing := filterRecords(records, candidate.Address, candidate.RecordType)
	if recordsMatch(existing, candidate, c.config.TTLSeconds) {
		return stateUnchanged, "target and TTL already match", nil
	}

	*mutated = true
	_, err = c.request(ctx, "api/zones/records/add", url.Values{
		"domain":    {candidate.Address},
		"zone":      {c.config.Zone},
		"type":      {candidate.RecordType},
		"ttl":       {strconv.Itoa(c.config.TTLSeconds)},
		"overwrite": {"true"},
		"ipAddress": {candidate.IP},
		"ptr":       {"false"},
	})
	if err != nil {
		return "", "", err
	}
	confirmed, err := c.records(ctx, candidate.Address)
	if err != nil {
		return "", "", err
	}
	if !recordsMatch(filterRecords(confirmed, candidate.Address, candidate.RecordType), candidate, c.config.TTLSeconds) {
		return "", "", apiError("api", "Technitium accepted the mutation but confirmation did not match")
	}
	if len(existing) == 0 {
		return stateCreated, "record created", nil
	}
	return stateUpdated, "record set reconciled", nil
}

// All records stored for one name
func (c *TechnitiumClient) records(ctx context.Context, domain string) ([]map[string]any, error) {
	payload, err := c.request(ctx, "api/zones/records/get", url.Values{
		"domain": {domain}, "zone": {c.config.Zone}, "listZone": {"false"},
	})
	if err != nil {
		return nil, err
	}
	response, _ := payload["response"].(map[string]any)
	items, ok := response["records"].([]any)
	if !ok {
		return nil, apiError("api", "Technitium returned an invalid records response")
	}
	records := make([]map[string]any, len(items))
	for index, item := range items {
		if records[index], ok = item.(map[string]any); !ok {
			return nil, apiError("api", "Technitium returned an invalid records response")
		}
	}
	return records, nil
}

// Form POST with bearer token; the token never appears in the URL
func (c *TechnitiumClient) request(ctx context.Context, path string, data url.Values) (map[string]any, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.config.BaseURL+"/"+path, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, apiError("api", "cannot build Technitium request")
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Authorization", "Bearer "+c.config.APIToken)
	response, err := c.http.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var wrapped *url.Error
		if errors.As(err, &wrapped) {
			err = wrapped.Err
		}
		return nil, apiError("connection", "cannot connect to Technitium: %v", err)
	}
	defer response.Body.Close()

	switch {
	case response.StatusCode == http.StatusUnauthorized:
		return nil, apiError("authentication", "Technitium rejected the API token")
	case response.StatusCode == http.StatusForbidden:
		return nil, apiError("permission", "Technitium denied the requested zone operation")
	case response.StatusCode >= 400:
		return nil, apiError("api", "Technitium returned HTTP %d", response.StatusCode)
	}
	var document any
	if err := json.NewDecoder(response.Body).Decode(&document); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, apiError("api", "Technitium returned a non-JSON response")
	}
	payload, ok := document.(map[string]any)
	if !ok {
		return nil, apiError("api", "Technitium returned an invalid JSON response")
	}
	switch payload["status"] {
	case "ok":
		return payload, nil
	case "invalid-token":
		return nil, apiError("authentication", "Technitium rejected the API token")
	}
	detail, _ := payload["errorMessage"].(string)
	if strings.TrimSpace(detail) == "" {
		detail = "API status was not ok"
	}
	if lower := strings.ToLower(detail); strings.Contains(lower, "permission") || strings.Contains(lower, "access denied") {
		return nil, apiError("permission", "Technitium denied the operation: %s", detail)
	}
	return nil, apiError("api", "Technitium API error: %s", detail)
}

func inZone(name, zone string) bool {
	return name == zone || strings.HasSuffix(name, "."+zone)
}

// Records of one type stored exactly at the name
func filterRecords(records []map[string]any, address, recordType string) []map[string]any {
	var matching []map[string]any
	for _, record := range records {
		name, _ := record["name"].(string)
		kind, _ := record["type"].(string)
		if normalized, err := normalizeDNSName(name); err == nil && normalized == address && strings.ToUpper(kind) == recordType {
			matching = append(matching, record)
		}
	}
	return matching
}

// Exactly one enabled record with the wanted target and TTL
func recordsMatch(records []map[string]any, candidate Candidate, ttl int) bool {
	if len(records) != 1 {
		return false
	}
	record := records[0]
	if record["disabled"] == true || record["ttl"] != float64(ttl) {
		return false
	}
	data, _ := record["rData"].(map[string]any)
	value, _ := data["ipAddress"].(string)
	stored, err := netip.ParseAddr(value)
	wanted, _ := netip.ParseAddr(candidate.IP)
	return err == nil && stored == wanted
}
