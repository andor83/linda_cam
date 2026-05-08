// Package correction implements the classifier-name correction rules
// (Settings → Classifier corrections). The same logic is used both on the
// hot inference path (Detector.classifyBirds) and by the retroactive
// apply-rules API endpoint, so it lives here as a small free-function pair.
package correction

import (
	"regexp"
	"strings"

	"github.com/linda/linda_cam/internal/config"
)

// Compile returns a parallel slice of compiled regexes for the rules that
// have Regex=true. Bad patterns become nil — the corresponding rule is
// effectively skipped at apply time. Each pattern gets an automatic `(?i)`
// prefix so regex matching is case-insensitive (matching the literal-rule
// behavior).
func Compile(rules []config.CorrectionRule) []*regexp.Regexp {
	out := make([]*regexp.Regexp, len(rules))
	for i, r := range rules {
		if !r.Regex || strings.TrimSpace(r.Detected) == "" {
			continue
		}
		if re, err := regexp.Compile("(?i)" + r.Detected); err == nil {
			out[i] = re
		}
	}
	return out
}

// Apply runs rules against name and returns the first match's correction.
// regexes must be the slice produced by Compile(rules) (parallel index).
// Returns name unchanged when no rule matches.
func Apply(name string, rules []config.CorrectionRule, regexes []*regexp.Regexp) string {
	if len(rules) == 0 {
		return name
	}
	trimmed := strings.TrimSpace(name)
	for i, r := range rules {
		if r.Regex {
			if i < len(regexes) && regexes[i] != nil && regexes[i].MatchString(name) {
				return r.Correction
			}
			continue
		}
		if strings.EqualFold(trimmed, strings.TrimSpace(r.Detected)) {
			return r.Correction
		}
	}
	return name
}
