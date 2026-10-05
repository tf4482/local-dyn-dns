// local-dyn-dns synchronizes PostgreSQL host records with Technitium DNS.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/term"
)

const help = projectName + ` — synchronize PostgreSQL hosts to Technitium DNS

Usage:
  ` + projectName + `
  ` + projectName + ` --help | -h | help

Configuration:
  Uses the first existing file without merging:
  1. config.yml beside the executable
  2. ~/.config/` + projectName + `/config.yml
  Missing configuration creates the user template and exits 1. Help never creates it.

Selection:
  Boolean/text true rows are active; false and NULL rows are inactive. Active rows need
  a valid IP and concrete DNS name. Priority is ignored for a unique case-sensitive
  Address. Repeated exact Addresses require finite priorities; the highest value wins.
  Invalid rows are reported and skipped without failing the run.

DNS behavior:
  IPv4 selects A; IPv6 selects AAAA. The selected type is created, updated to one target,
  or left unchanged. Other names/types and the opposite address family are preserved.
  CNAME conflicts and names outside the configured zone are skipped as errors. No record
  is deleted because a database row is inactive or absent. Each name has a hard deadline.

Exit codes:
  0   All eligible names synchronized, including empty/inactive-only tables
  1   Configuration, ambiguity, database, dependency, or DNS/API error
  130 Interrupted by the user
`

// Colored line output
type console struct {
	out   io.Writer
	color bool
}

// Framed startup banner
func (c console) header() {
	title := "  ⚙️  " + projectName + " — starting  "
	border := strings.Repeat("─", utf8.RuneCountInString(title))
	fmt.Fprintln(c.out)
	for _, line := range []string{"╭" + border + "╮", "│" + title + "│", "╰" + border + "╯"} {
		c.print(line, "36")
	}
}

func (c console) line(icon, label, detail, color string) {
	text := icon + " " + label
	if detail != "" {
		text += ": " + detail
	}
	c.print(text, color)
}

func (c console) print(text, color string) {
	if c.color && color != "" {
		text = "\033[" + color + "m" + text + "\033[0m"
	}
	fmt.Fprintln(c.out, text)
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(arguments []string) int {
	if len(arguments) == 1 && (arguments[0] == "-h" || arguments[0] == "--help" || arguments[0] == "help") {
		fmt.Print(help)
		return 0
	}
	if len(arguments) > 0 {
		fmt.Fprintf(os.Stderr, "❌ Unknown arguments: %s\n", strings.Join(arguments, " "))
		fmt.Fprintf(os.Stderr, "Run '%s --help' for usage.\n", projectName)
		return 1
	}

	_, noColor := os.LookupEnv("NO_COLOR")
	out := console{os.Stdout, term.IsTerminal(int(os.Stdout.Fd())) && !noColor}

	// First Ctrl-C cancels cleanly, second one kills
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	context.AfterFunc(ctx, stop)

	code := synchronize(ctx, out)
	if ctx.Err() != nil {
		out.line("🛑", "Interrupted", "pending DNS work cancelled", "33")
		return 130
	}
	return code
}

// Load, select, synchronize, report
func synchronize(ctx context.Context, out console) int {
	started := time.Now()
	out.header()
	// Early failure with an all-zero summary
	abort := func(label, detail string) int {
		out.line("❌", label, detail, "31")
		printSummary(out, Selection{}, nil, 1, started)
		return 1
	}

	binaryDir, err := executableDir()
	if err != nil {
		return abort("Configuration error", err.Error())
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return abort("Configuration error", err.Error())
	}
	path, config, err := loadConfig(binaryDir, home)
	var created *configCreatedError
	if errors.As(err, &created) {
		out.line("📝", "Configuration created", created.path, "33")
		out.line("✏️", "Edit required", "database name/user/password and Technitium api_token/zone", "33")
		printSummary(out, Selection{}, nil, 1, started)
		return 1
	}
	if err != nil {
		return abort("Configuration error", err.Error())
	}
	out.line("⚙️", "Configuration", path, "36")

	rows, err := readHostRows(ctx, config.Database, config.Backups)
	if ctx.Err() != nil {
		return 130
	}
	if err != nil {
		return abort("Database error", redact(err.Error(), config))
	}

	selection := selectRecords(rows)
	for _, issue := range selection.Invalid {
		out.line("⚠️", fmt.Sprintf("Invalid row %d", issue.Row), issue.Reason, "33")
	}
	for _, ambiguous := range selection.Ambiguous {
		out.line("⏭️", "Skipped "+ambiguous.Address, ambiguous.Reason+": "+strings.Join(ambiguous.IPs, ", "), "33")
	}

	results := synchronizeAll(ctx, selection.Selected, newTechnitiumClient(config.Technitium), config.Settings)
	if ctx.Err() != nil {
		return 130
	}
	for _, result := range results {
		printSyncResult(out, result, config)
	}
	errorCount := printSummary(out, selection, results, 0, started)
	if len(selection.Ambiguous) > 0 || errorCount > 0 {
		return 1
	}
	return 0
}

func printSyncResult(out console, result SyncResult, config Config) {
	icon, color := "❌", "31"
	switch result.State {
	case stateCreated:
		icon, color = "✅", "32"
	case stateUpdated:
		icon, color = "🔄", "32"
	case stateUnchanged:
		icon, color = "✔️", "36"
	case stateSkipped:
		icon, color = "⏭️", "33"
	}
	detail := redact(result.Detail, config)
	if result.ErrorKind != "" {
		detail = result.ErrorKind + ": " + detail
	}
	label := strings.ToUpper(result.State[:1]) + result.State[1:]
	out.line(icon, fmt.Sprintf("%s %s %s", label, result.Candidate.Address, result.Candidate.RecordType), detail, color)
}

// Totals line; returns the operational error count
func printSummary(out console, selection Selection, results []SyncResult, extraErrors int, started time.Time) int {
	counts := map[string]int{}
	errorCount := extraErrors
	for _, result := range results {
		counts[result.State]++
		if result.ErrorKind != "" {
			errorCount++
		}
	}
	out.line("📊", "Summary", fmt.Sprintf(
		"rows=%d, inactive=%d, invalid=%d, non-selected=%d, ambiguous=%d, created=%d, updated=%d, unchanged=%d, operational-errors=%d, runtime=%.2fs",
		selection.RowsRead, selection.Inactive, len(selection.Invalid), selection.NonSelected, len(selection.Ambiguous),
		counts[stateCreated], counts[stateUpdated], counts[stateUnchanged], errorCount, time.Since(started).Seconds()), "35")
	return errorCount
}

// Mask passwords and the API token in messages
func redact(message string, config Config) string {
	secrets := []string{config.Database.Password, config.Technitium.APIToken}
	for _, backup := range config.Backups {
		secrets = append(secrets, backup.Password)
	}
	// Longest first so overlapping secrets vanish completely
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	for _, secret := range secrets {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[REDACTED]")
		}
	}
	return message
}
