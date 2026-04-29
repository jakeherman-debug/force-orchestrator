package claude

import (
	"database/sql"
	"fmt"
	"log"
	"regexp"
	"strings"
	"sync"

	"force-orchestrator/internal/store"
)

// ScrubInbound is the inbound chokepoint applied to every Claude CLI prompt
// before it leaves the orchestrator process. It is the dual of
// store.RedactSecrets — that function scrubs OUTBOUND content (webhooks,
// telemetry, mail bodies). ScrubInbound scrubs INBOUND content (prompts +
// system instructions on their way to claude -p), preventing secrets that
// astromechs accidentally read from a target repo (e.g. .env files,
// PEM-encoded private keys, GCP service-account JSON) from reaching
// Anthropic's prompt cache.
//
// The function is fail-closed: if anything is redacted, the call proceeds
// with the redacted prompt. There is no "warn but send original" path —
// redaction is enforcement, not advisory (T0-2 anti-cheat).
//
// Return value: (scrubbedPrompt, redactionCount). redactionCount is the
// total number of distinct match-and-replace events across all patterns,
// used by the caller to drive the operator-mail dedup alert.
func ScrubInbound(prompt string) (string, int) {
	if prompt == "" {
		return prompt, 0
	}
	count := 0
	s := prompt

	// 1. Outbound-shared patterns first. store.RedactSecrets covers GH PATs
	// (ghp_, gho_, etc.), bearer tokens, and URL-embedded basic-auth. We
	// count its activity by counting NEW [REDACTED] markers introduced.
	origMarkers := strings.Count(s, "[REDACTED]")
	s = store.RedactSecrets(s)
	count += strings.Count(s, "[REDACTED]") - origMarkers

	// 2. GCP service-account JSON private_key field. Run BEFORE the bare
	// PEM regex so the entire JSON value (including quotes) collapses to
	// "private_key":"[REDACTED]" rather than leaving "private_key":"
	// [REDACTED PEM PRIVATE KEY]" — the latter would still leak the JSON
	// structure of the credential file.
	if matches := gcpPrivateKeyRe.FindAllStringIndex(s, -1); len(matches) > 0 {
		count += len(matches)
		s = gcpPrivateKeyRe.ReplaceAllString(s, `"private_key":"[REDACTED]"`)
	}

	// 3. Multiline PEM blocks. (?s) is dotall so `.` matches newlines.
	// The block markers are the most common shape for accidentally
	// committed private keys in repos; multiline coverage is non-negotiable
	// per the T0-2 anti-cheat directives.
	if matches := pemBlockRe.FindAllStringIndex(s, -1); len(matches) > 0 {
		count += len(matches)
		s = pemBlockRe.ReplaceAllString(s, "[REDACTED PEM PRIVATE KEY]")
	}

	// 4. .env-shape assignments where the LHS contains a sensitive token.
	// LHS is the canonical SHELL_VARIABLE form ([A-Z_][A-Z0-9_]*); the
	// sensitive substrings are wrapped to avoid greedy [A-Z_][A-Z0-9_]*
	// swallowing the discriminator before the alternation runs.
	if matches := envAssignmentRe.FindAllStringSubmatchIndex(s, -1); len(matches) > 0 {
		count += len(matches)
		s = envAssignmentRe.ReplaceAllString(s, "${1}=[REDACTED]")
	}

	// 5. AWS access key ID. store.RedactSecrets does not cover this; the
	// AKIA prefix + 16 base32 chars is unambiguous enough to redact on
	// sight without false positives in prose.
	if matches := awsAccessKeyRe.FindAllStringIndex(s, -1); len(matches) > 0 {
		count += len(matches)
		s = awsAccessKeyRe.ReplaceAllString(s, "[REDACTED]")
	}

	// 6. Bearer / GH PAT — store.RedactSecrets covers these but uses
	// substring anchors that miss tokens introduced by inbound-only
	// shapes (e.g. "Authorization=ghp_xxxxxxxxxx" inside an .env line
	// the env-assignment pattern already collapsed). The bearer/GH-PAT
	// regex below catches the rare leak that survives steps 1 and 4.
	if matches := bearerOrPATRe.FindAllStringIndex(s, -1); len(matches) > 0 {
		// Some matches will be against the literal "[REDACTED]" tail of
		// a prior replacement; collapse them into the count but the
		// replacement is harmless (idempotent string-wise).
		count += len(matches)
		s = bearerOrPATRe.ReplaceAllString(s, "[REDACTED]")
	}

	return s, count
}

// pemBlockRe matches a PEM-encoded private-key block including the BEGIN
// and END markers. Variants seen in practice: "RSA PRIVATE KEY", "EC
// PRIVATE KEY", "DSA PRIVATE KEY", "OPENSSH PRIVATE KEY", and the bare
// "PRIVATE KEY" (PKCS#8). The (?s) flag makes `.` span newlines so the
// non-greedy body matches the entire base64 payload.
//
// Important: the END marker matches `[^-]+` rather than a fixed key-type
// label so the regex tolerates inputs where the BEGIN type doesn't echo
// in the END (e.g. "BEGIN RSA PRIVATE KEY" / "END PRIVATE KEY" — rare
// but seen).
var pemBlockRe = regexp.MustCompile(
	`(?s)-----BEGIN (?:RSA |EC |DSA |OPENSSH |)PRIVATE KEY-----.*?-----END [^-]+PRIVATE KEY-----`,
)

// gcpPrivateKeyRe matches the JSON shape of a GCP service-account key
// file's private_key field. The `[\s\S]*?` is the JSON-aware dotall
// equivalent for use without the (?s) flag — needed because we want
// the body to span newlines but the rest of the regex (the "private_key"
// keyword) should not be interpreted under (?s) anchoring rules.
var gcpPrivateKeyRe = regexp.MustCompile(
	`"private_key"\s*:\s*"-----BEGIN[\s\S]*?-----END[^"]*"`,
)

// envAssignmentRe matches a SHELL_VARIABLE=value pair where the variable
// name contains one of the sensitive substrings. The structure is:
//
//	(word boundary)
//	(optional [A-Z_][A-Z0-9_]*_ prefix)
//	(API_KEY|SECRET|TOKEN|PASSWORD|PRIVATE_KEY|CREDENTIAL|AUTH)
//	(optional _[A-Z0-9_]+ suffix)
//	=
//	(value to end-of-line)
//
// The split into prefix/discriminator/suffix is necessary because Go's
// RE2 has no backtracking; a single greedy [A-Z_][A-Z0-9_]* would
// swallow the entire name and leave the alternation with nothing to
// match. \b at the start prevents false-positive matches mid-token (so
// "XAPI_KEY=…" does not get mis-split — the `_` requirement on the
// prefix anchors the discriminator to its own word).
//
// We use \b rather than (?m)^ so the pattern catches mid-line shapes
// like "First: API_KEY=…" — these arise when an astromech's tool output
// embeds an .env line in a prose narration.
//
// Group 1 captures the LHS so the replacement can preserve "MY_API_KEY"
// while collapsing the value to [REDACTED].
var envAssignmentRe = regexp.MustCompile(
	`\b((?:[A-Z_][A-Z0-9_]*_)?(?:API_KEY|SECRET|TOKEN|PASSWORD|PRIVATE_KEY|CREDENTIAL|AUTH)(?:_[A-Z0-9_]+)?)\s*=\s*[^\n\r]+`,
)

// awsAccessKeyRe matches an AWS access key ID. Format is the AKIA prefix
// (or its rare AKID/ASIA cousins) plus exactly 16 base32 characters. We
// stick to AKIA only to avoid false positives on unrelated all-caps
// substrings; the secret_access_key portion is caught by envAssignmentRe.
var awsAccessKeyRe = regexp.MustCompile(`AKIA[0-9A-Z]{16}`)

// bearerOrPATRe is a defense-in-depth supplement to store.RedactSecrets's
// own bearer/PAT patterns. The store regex preserves the "Bearer " prefix
// (so log lines stay readable); this one collapses the entire token-and-
// prefix tuple to [REDACTED] when the pattern surfaces inside an
// already-scrubbed prompt context.
var bearerOrPATRe = regexp.MustCompile(
	`(?:ghp_|github_pat_|Bearer\s+)[A-Za-z0-9_\-./=+~]{20,}`,
)

// ── Operator-mail dedup state ──────────────────────────────────────────
//
// High-volume inbound redactions imply a repo-hygiene problem (the
// operator's astromech is reading secret-laden files). One mail every N
// redactions is enough to surface the issue; a per-event mail would
// drown the operator's inbox during a noisy day.
//
// State lives in three SystemConfig keys:
//
//	inbound_redact_total_count        — running total of redactions
//	inbound_redact_alert_threshold    — N (default 10)
//	inbound_redact_last_alert_count   — total at the last mail emit
//
// When (running_total - last_alert_count) >= threshold, one mail fires
// and last_alert_count is bumped to running_total.

const (
	cfgInboundRedactTotal     = "inbound_redact_total_count"
	cfgInboundRedactThreshold = "inbound_redact_alert_threshold"
	cfgInboundRedactLastAlert = "inbound_redact_last_alert_count"
	defaultInboundAlertEvery  = 10
)

// inboundRedactMu serializes the read-modify-write on the three SystemConfig
// rows so two agents scrubbing in parallel cannot cross-fire alerts. The
// underlying SystemConfig table has no row-level locking; a Go-side mutex
// is the cheapest correct guard.
var inboundRedactMu sync.Mutex

// inboundRedactDB is the daemon-supplied DB handle used by the wrappers
// in claude.go to record the redaction count and (when threshold is
// crossed) emit operator mail. Tests inject nil so unit-test scrubs do
// not write SystemConfig rows.
var inboundRedactDB *sql.DB

// SetInboundRedactDB wires the SystemConfig-backed alerter to a live DB.
// Called once at daemon startup, after store.InitHolocronDSN. Passing nil
// disables alerting (used by unit tests that exercise ScrubInbound in
// isolation).
func SetInboundRedactDB(db *sql.DB) {
	inboundRedactMu.Lock()
	defer inboundRedactMu.Unlock()
	inboundRedactDB = db
}

// recordInboundRedact updates the SystemConfig dedup state and, if the
// threshold is crossed, sends a single operator mail summarising the
// recent burst. agentName + taskID are echoed back to the operator so
// they can correlate the alert with the LLM call that triggered it.
//
// Returns an error per the CLAUDE.md "every new mutator returns error"
// policy. Callers in the claude wrappers check the error and log a
// recovery hint; an alerter outage MUST NOT block the underlying LLM
// call from proceeding (the redaction itself is already in effect).
func recordInboundRedact(db *sql.DB, agentName string, taskID int, count int) error {
	if db == nil || count <= 0 {
		return nil
	}
	inboundRedactMu.Lock()
	defer inboundRedactMu.Unlock()

	totalStr := store.GetConfig(db, cfgInboundRedactTotal, "0")
	threshStr := store.GetConfig(db, cfgInboundRedactThreshold, fmt.Sprintf("%d", defaultInboundAlertEvery))
	lastStr := store.GetConfig(db, cfgInboundRedactLastAlert, "0")

	var total, threshold, last int
	fmt.Sscanf(totalStr, "%d", &total)
	fmt.Sscanf(threshStr, "%d", &threshold)
	fmt.Sscanf(lastStr, "%d", &last)
	if threshold <= 0 {
		threshold = defaultInboundAlertEvery
	}

	total += count
	if _, err := db.Exec(
		`INSERT OR REPLACE INTO SystemConfig (key, value) VALUES (?, ?)`,
		cfgInboundRedactTotal, fmt.Sprintf("%d", total),
	); err != nil {
		return fmt.Errorf("recordInboundRedact: persist total: %w", err)
	}

	if total-last < threshold {
		return nil
	}
	// Threshold crossed — emit one mail and advance last_alert.
	if _, err := db.Exec(
		`INSERT OR REPLACE INTO SystemConfig (key, value) VALUES (?, ?)`,
		cfgInboundRedactLastAlert, fmt.Sprintf("%d", total),
	); err != nil {
		return fmt.Errorf("recordInboundRedact: advance last_alert: %w", err)
	}
	subject := "[INBOUND REDACT — repo hygiene]"
	body := fmt.Sprintf(
		"Inbound prompt scrubbing has redacted %d items since the last alert (running total: %d). "+
			"Most recent trigger: agent=%q task=%d added %d redactions to the running total.\n\n"+
			"This usually means an astromech read a secret-bearing file (.env, credentials.json, "+
			"PEM key, etc.) and its content was about to flow into a Claude prompt. The redaction "+
			"is in effect — the prompt sent to Claude has [REDACTED] markers in place of the secrets — "+
			"but the underlying repo hygiene problem should be addressed:\n\n"+
			"  • Add a .forceignore at the target repo root to skip secret files at read time.\n"+
			"  • Audit the repo's git history; if a real secret was committed, rotate it.\n"+
			"  • Lower 'inbound_redact_alert_threshold' in SystemConfig to surface this sooner.\n",
		total-last+threshold, total, agentName, taskID, count,
	)
	store.SendMail(db, "claude-inbound-redact", "operator", subject, body, taskID, store.MailTypeAlert)
	return nil
}

// observeInboundRedact is the single hot-path observer called by the
// claude CLI wrappers after each ScrubInbound call. It is allocation-free
// when count == 0 (the common case).
func observeInboundRedact(agentName string, taskID int, count int) {
	if count <= 0 {
		return
	}
	log.Printf("[INBOUND REDACT] agent=%s task=%d redactions=%d", agentName, taskID, count)
	if err := recordInboundRedact(inboundRedactDB, agentName, taskID, count); err != nil {
		// Per CLAUDE.md "no silent failures": surface the alerter
		// outage in the daemon log. The redaction itself is already
		// in effect; an alerter failure must not block the LLM call.
		log.Printf("[INBOUND REDACT] alert state update failed: %v — redaction still applied to prompt", err)
	}
}
