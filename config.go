package main

import (
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	projectName    = "local-dyn-dns"
	configFilename = "config.yml"
	placeholder    = "CHANGE_ME"
	maxTTL         = 4_294_967_295

	// Fixed table layout
	dbSchema = "public"
	dbTable  = "hosts"
)

const exampleConfig = `database:
  host: "127.0.0.1"
  port: 5432
  name: "monitoring"
  user: "monitoring"
  password: "CHANGE_ME"
  # Optional ordered failover targets (maximum of three).
  backups: []

technitium:
  base_url: "http://127.0.0.1:5380"
  api_token: "CHANGE_ME"
  zone: "CHANGE_ME"
  ttl_seconds: 60

settings:
  dns_concurrency: 8
  dns_timeout_seconds: 5
`

// PostgreSQL connection target
type DatabaseConfig struct {
	Host     string
	Port     int
	Name     string
	User     string
	Password string
}

// Technitium API target
type TechnitiumConfig struct {
	BaseURL    string
	APIToken   string
	Zone       string
	TTLSeconds int
}

// DNS limits and per-name deadline
type Settings struct {
	Concurrency int
	Timeout     time.Duration
}

// Validated configuration
type Config struct {
	Database   DatabaseConfig
	Backups    []DatabaseConfig
	Technitium TechnitiumConfig
	Settings   Settings
}

// Signal for a freshly written template
type configCreatedError struct{ path string }

func (e *configCreatedError) Error() string {
	return "created configuration template at " + e.path
}

// Directory of the running binary, symlinks resolved
func executableDir() (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(executable); err == nil {
		executable = resolved
	}
	return filepath.Dir(executable), nil
}

// First existing file wins; no merging
func loadConfig(binaryDir, home string) (string, Config, error) {
	userPath := filepath.Join(home, ".config", projectName, configFilename)
	selected := ""
	for _, candidate := range []string{filepath.Join(binaryDir, configFilename), userPath} {
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
			selected = candidate
			break
		}
	}
	if selected == "" {
		if err := createTemplate(userPath); err != nil {
			return "", Config{}, err
		}
		return "", Config{}, &configCreatedError{userPath}
	}

	data, err := os.ReadFile(selected)
	if err != nil {
		return "", Config{}, fmt.Errorf("cannot read %s: %v", selected, err)
	}
	var document any
	if err := yaml.Unmarshal(data, &document); err != nil {
		return "", Config{}, fmt.Errorf("invalid YAML in %s%s", selected, yamlLocation(err))
	}
	config, err := parseConfig(document)
	return selected, config, err
}

var yamlLine = regexp.MustCompile(`line (\d+)`)

// Line number only; never echoes file content
func yamlLocation(err error) string {
	if match := yamlLine.FindStringSubmatch(err.Error()); match != nil {
		return " near line " + match[1]
	}
	return ""
}

// User-only template with dummy values; an existing file is kept
func createTemplate(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("cannot create configuration template at %s: %v", path, err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("cannot create configuration template at %s: %v", path, err)
	}
	_, err = file.WriteString(exampleConfig)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("cannot create configuration template at %s: %v", path, err)
	}
	return nil
}

// Validation failure raised while parsing
type configError struct{ error }

func fail(format string, args ...any) {
	panic(configError{fmt.Errorf(format, args...)})
}

// Strict validation of the decoded YAML document
func parseConfig(document any) (config Config, err error) {
	defer func() {
		switch failure := recover().(type) {
		case nil:
		case configError:
			config, err = Config{}, failure.error
		default:
			panic(failure)
		}
	}()

	root := mapping(document, "configuration")
	exactKeys(root, "configuration", "database", "technitium", "settings")
	database := mapping(root["database"], "database")
	technitium := mapping(root["technitium"], "technitium")
	settings := mapping(root["settings"], "settings")
	databaseKeys := []string{"host", "port", "name", "user", "password"}
	backups, hasBackups := database["backups"]
	if hasBackups {
		exactKeys(database, "database", append(databaseKeys, "backups")...)
	} else {
		exactKeys(database, "database", databaseKeys...)
	}
	exactKeys(technitium, "technitium", "base_url", "api_token", "zone", "ttl_seconds")
	exactKeys(settings, "settings", "dns_concurrency", "dns_timeout_seconds")

	if hasBackups {
		entries, ok := backups.([]any)
		if !ok {
			fail("database.backups must be a YAML list")
		}
		if len(entries) > 3 {
			fail("database.backups must contain no more than three entries")
		}
		for index, entry := range entries {
			field := fmt.Sprintf("database.backups[%d]", index)
			backup := mapping(entry, field)
			exactKeys(backup, field, databaseKeys...)
			config.Backups = append(config.Backups, databaseEndpoint(backup, field))
		}
	}
	config.Database = databaseEndpoint(database, "database")
	config.Technitium = TechnitiumConfig{
		BaseURL:    baseURL(technitium["base_url"]),
		APIToken:   text(technitium["api_token"], "technitium.api_token"),
		Zone:       zone(technitium["zone"]),
		TTLSeconds: integer(technitium["ttl_seconds"], "technitium.ttl_seconds", 0, maxTTL),
	}
	config.Settings = Settings{
		Concurrency: integer(settings["dns_concurrency"], "settings.dns_concurrency", 1, 0),
		Timeout:     positiveSeconds(settings["dns_timeout_seconds"], "settings.dns_timeout_seconds"),
	}
	return config, nil
}

func databaseEndpoint(values map[string]any, field string) DatabaseConfig {
	return DatabaseConfig{
		Host:     text(values["host"], field+".host"),
		Port:     integer(values["port"], field+".port", 1, 65535),
		Name:     text(values["name"], field+".name"),
		User:     text(values["user"], field+".user"),
		Password: text(values["password"], field+".password"),
	}
}

func mapping(value any, field string) map[string]any {
	result, ok := value.(map[string]any)
	if !ok {
		fail("%s must be a YAML mapping", field)
	}
	return result
}

// Reject missing and unknown keys
func exactKeys(values map[string]any, field string, expected ...string) {
	known := map[string]bool{}
	var missing, unknown []string
	for _, key := range expected {
		known[key] = true
		if _, ok := values[key]; !ok {
			missing = append(missing, key)
		}
	}
	for key := range values {
		if !known[key] {
			unknown = append(unknown, key)
		}
	}
	sort.Strings(missing)
	sort.Strings(unknown)
	var details []string
	if missing != nil {
		details = append(details, "missing: "+strings.Join(missing, ", "))
	}
	if unknown != nil {
		details = append(details, "unknown: "+strings.Join(unknown, ", "))
	}
	if details != nil {
		fail("%s keys are invalid (%s)", field, strings.Join(details, "; "))
	}
}

// Trimmed non-empty string without placeholder
func text(value any, field string) string {
	raw, ok := value.(string)
	result := strings.TrimSpace(raw)
	if !ok || result == "" {
		fail("%s must be a non-empty string", field)
	}
	if strings.Contains(result, "\x00") {
		fail("%s must not contain a null character", field)
	}
	if strings.EqualFold(result, placeholder) {
		fail("%s still contains the %s placeholder", field, placeholder)
	}
	return result
}

// Bounded integer; maximum 0 means unbounded
func integer(value any, field string, minimum, maximum int) int {
	number, ok := value.(int)
	if !ok {
		fail("%s must be an integer", field)
	}
	if maximum > 0 && (number < minimum || number > maximum) {
		fail("%s must be between %d and %d", field, minimum, maximum)
	}
	if number < minimum {
		fail("%s must be >= %d", field, minimum)
	}
	return number
}

// Positive finite seconds as duration
func positiveSeconds(value any, field string) time.Duration {
	var seconds float64
	switch number := value.(type) {
	case int:
		seconds = float64(number)
	case float64:
		seconds = number
	default:
		fail("%s must be a number", field)
	}
	if !(seconds > 0) || math.IsInf(seconds, 0) {
		fail("%s must be finite and greater than zero", field)
	}
	// Clamp to avoid duration overflow
	return time.Duration(math.Min(seconds, 86400) * float64(time.Second))
}

// HTTP(S) base URL without credentials, query, or fragment
func baseURL(value any) string {
	result := text(value, "technitium.base_url")
	parsed, err := url.Parse(result)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		fail("technitium.base_url must be an HTTP(S) base URL without credentials, query, or fragment")
	}
	if port := parsed.Port(); port != "" {
		if number, err := strconv.Atoi(port); err != nil || number < 1 || number > 65535 {
			fail("technitium.base_url port must be between 1 and 65535")
		}
	}
	return strings.TrimRight(result, "/")
}

func zone(value any) string {
	result, err := normalizeDNSName(text(value, "technitium.zone"))
	if err != nil {
		fail("technitium.zone is invalid: %v", err)
	}
	return result
}
