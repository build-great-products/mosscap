// Package doctor defines non-destructive, stable diagnostic reports.
package doctor

// CheckStatus is deliberately a closed Phase 0 vocabulary.
type CheckStatus string

const (
	Pass    CheckStatus = "pass"
	Warn    CheckStatus = "warn"
	Fail    CheckStatus = "fail"
	Skipped CheckStatus = "skipped"
)

// Check is an action-oriented, secret-safe diagnostic result.
type Check struct {
	Name    string      `json:"name"`
	Status  CheckStatus `json:"status"`
	Message string      `json:"message"`
	Action  string      `json:"action,omitempty"`
}

// Report is the stable doctor_report result.
type Report struct {
	Overall CheckStatus `json:"overall"`
	Checks  []Check     `json:"checks"`
}

// Overall derives a report summary, treating unavailable checks as skipped.
func Overall(checks []Check) CheckStatus {
	seenWarn, seenPass := false, false
	for _, c := range checks {
		switch c.Status {
		case Fail:
			return Fail
		case Warn:
			seenWarn = true
		case Pass:
			seenPass = true
		}
	}
	if seenWarn {
		return Warn
	}
	if seenPass {
		return Pass
	}
	return Skipped
}
