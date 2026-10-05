package main

import (
	"errors"
	"math/big"
	"net/netip"
	"regexp"
	"sort"
	"strings"

	"golang.org/x/net/idna"
)

var (
	dnsLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	decimal  = regexp.MustCompile(`^[+-]?(\d+\.?\d*|\.\d+)([eE][+-]?\d+)?$`)
	infinite = regexp.MustCompile(`(?i)^[+-]?(inf|infinity|s?nan)$`)
)

// Unvalidated row as stored
type HostRow struct {
	Status   any
	IP       any
	Address  any
	Priority any
}

// DNS record to synchronize
type Candidate struct {
	Address    string
	IP         string
	RecordType string
}

// Skipped row with its reason
type RecordIssue struct {
	Row    int
	Reason string
}

// Name whose rows disagree on the target
type Ambiguous struct {
	Address string
	IPs     []string
	Reason  string
}

// Outcome of row selection
type Selection struct {
	RowsRead    int
	Inactive    int
	NonSelected int
	Invalid     []RecordIssue
	Ambiguous   []Ambiguous
	Selected    []Candidate
}

// Active row waiting for priority comparison
type activeRow struct {
	number   int
	address  string
	ip       string
	kind     string
	priority any
}

// Candidate with its compared priority
type ranked struct {
	Candidate
	priority *big.Rat
	label    string
}

// Boolean or text true/false; NULL is inactive
func parseStatus(value any) (active, ok bool) {
	switch status := value.(type) {
	case nil:
		return false, true
	case bool:
		return status, true
	case string:
		switch strings.ToLower(strings.TrimSpace(status)) {
		case "true":
			return true, true
		case "false":
			return false, true
		}
	}
	return false, false
}

// Lower-case ASCII host name without trailing dot
func normalizeDNSName(value string) (string, error) {
	raw := strings.TrimSuffix(strings.TrimSpace(value), ".")
	if raw == "" || strings.HasPrefix(raw, "*.") || strings.ContainsAny(raw, "/:@?#") {
		return "", errors.New("Address must be a concrete DNS host name")
	}
	ascii, err := idna.Lookup.ToASCII(raw)
	if err != nil {
		return "", errors.New("Address is not a valid DNS host name")
	}
	normalized := strings.ToLower(ascii)
	if len(normalized) > 253 {
		return "", errors.New("Address exceeds 253 characters")
	}
	for _, label := range strings.Split(normalized, ".") {
		if !dnsLabel.MatchString(label) {
			return "", errors.New("Address contains an invalid DNS label")
		}
	}
	return normalized, nil
}

// Compressed address and matching record type; accepts text and inet values
func normalizeIP(value any) (string, string, error) {
	var address netip.Addr
	switch ip := value.(type) {
	case string:
		if strings.TrimSpace(ip) == "" {
			return "", "", errors.New("IP is missing or empty")
		}
		address, _ = netip.ParseAddr(strings.TrimSpace(ip))
	case netip.Addr:
		address = ip
	case netip.Prefix:
		if ip.IsSingleIP() {
			address = ip.Addr()
		}
	default:
		return "", "", errors.New("IP is missing or empty")
	}
	if !address.IsValid() || address.Zone() != "" {
		return "", "", errors.New("IP is not a valid IPv4 or IPv6 address")
	}
	if address.Is4() {
		return address.String(), "A", nil
	}
	return address.String(), "AAAA", nil
}

// Finite decimal priority and its trimmed text form
func normalizePriority(value any) (*big.Rat, string, error) {
	raw, ok := value.(string)
	if !ok {
		return nil, "", errors.New("priority is missing or not numeric")
	}
	raw = strings.TrimSpace(raw)
	if infinite.MatchString(raw) {
		return nil, "", errors.New("priority must be finite")
	}
	if !decimal.MatchString(raw) {
		return nil, "", errors.New("priority is not numeric")
	}
	priority, ok := new(big.Rat).SetString(raw)
	if !ok {
		return nil, "", errors.New("priority is not numeric")
	}
	return priority, raw, nil
}

func sortedIPs(candidates []ranked) []string {
	seen := map[string]bool{}
	var ips []string
	for _, candidate := range candidates {
		if !seen[candidate.IP] {
			seen[candidate.IP] = true
			ips = append(ips, candidate.IP)
		}
	}
	sort.Strings(ips)
	return ips
}

func sortedKeys[V any](groups map[string][]V) []string {
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// Pick one target per DNS name from the active rows
func selectRecords(rows []HostRow) Selection {
	selection := Selection{RowsRead: len(rows)}
	exactGroups := map[string][]activeRow{}
	for index, row := range rows {
		number := index + 1
		invalid := func(reason string) {
			selection.Invalid = append(selection.Invalid, RecordIssue{number, reason})
		}
		active, ok := parseStatus(row.Status)
		if !ok {
			invalid("status must be boolean true/false or text true/false")
			continue
		}
		if !active {
			selection.Inactive++
			continue
		}
		source, _ := row.Address.(string)
		source = strings.TrimSpace(source)
		if source == "" {
			invalid("Address is missing or empty")
			continue
		}
		address, err := normalizeDNSName(source)
		if err != nil {
			invalid(err.Error())
			continue
		}
		ip, kind, err := normalizeIP(row.IP)
		if err != nil {
			invalid(err.Error())
			continue
		}
		exactGroups[source] = append(exactGroups[source], activeRow{number, address, ip, kind, row.Priority})
	}

	// Repeated exact addresses compete by priority
	normalizedGroups := map[string][]ranked{}
	for _, source := range sortedKeys(exactGroups) {
		group := exactGroups[source]
		if len(group) == 1 {
			row := group[0]
			normalizedGroups[row.address] = append(normalizedGroups[row.address],
				ranked{Candidate: Candidate{row.address, row.ip, row.kind}})
			continue
		}
		var candidates, tied []ranked
		for _, row := range group {
			priority, label, err := normalizePriority(row.priority)
			if err != nil {
				selection.Invalid = append(selection.Invalid, RecordIssue{row.number, err.Error()})
				continue
			}
			candidate := ranked{Candidate{row.address, row.ip, row.kind}, priority, label}
			candidates = append(candidates, candidate)
			switch {
			case tied == nil || priority.Cmp(tied[0].priority) > 0:
				tied = []ranked{candidate}
			case priority.Cmp(tied[0].priority) == 0:
				tied = append(tied, candidate)
			}
		}
		if candidates == nil {
			continue
		}
		if ips := sortedIPs(tied); len(ips) > 1 {
			selection.Ambiguous = append(selection.Ambiguous, Ambiguous{tied[0].Address, ips,
				"exact Address tie at priority " + tied[0].label})
			selection.NonSelected += len(candidates)
			continue
		}
		normalizedGroups[tied[0].Address] = append(normalizedGroups[tied[0].Address], tied[0])
		selection.NonSelected += len(candidates) - 1
	}

	// Case and trailing-dot variants must agree on one target
	for _, address := range sortedKeys(normalizedGroups) {
		candidates := normalizedGroups[address]
		if ips := sortedIPs(candidates); len(ips) > 1 {
			selection.Ambiguous = append(selection.Ambiguous, Ambiguous{address, ips,
				"case-sensitive Address variants normalize to one DNS name"})
			selection.NonSelected += len(candidates)
			continue
		}
		selection.Selected = append(selection.Selected, candidates[0].Candidate)
		selection.NonSelected += len(candidates) - 1
	}
	return selection
}
