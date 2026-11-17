// Package secretscan finds text that looks like a credential: API keys with
// a known prefix, private key blocks, and long random values written right
// after words such as "token" or "password". It reports where they are and
// what kind they look like, never the matched text itself.
package secretscan

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
)

// Kinds reported in Finding.Kind.
const (
	KindAnthropicKey = "anthropic_key"
	KindOpenAIKey    = "openai_key"
	KindGitHubToken  = "github_token"
	KindSlackToken   = "slack_token"
	KindAWSAccessKey = "aws_access_key"
	KindPrivateKey   = "private_key"
	KindSecretValue  = "secret_value"
)

// Finding is one key-shaped span in the scanned text. Start and End are
// byte offsets (text[Start:End]). It deliberately carries no copy of the
// matched value.
type Finding struct {
	Kind       string
	Start, End int
}

// String shows the kind and position only.
func (f Finding) String() string { return fmt.Sprintf("%s at %d-%d", f.Kind, f.Start, f.End) }

type prefixRule struct {
	kind string
	re   *regexp.Regexp
	// body is the submatch index of the random part, checked by looksRandom;
	// 0 means the whole match is accepted as is.
	body int
}

var prefixRules = []prefixRule{
	{KindAnthropicKey, regexp.MustCompile(`\bsk-ant-(?:[a-z0-9]+-)?([A-Za-z0-9_-]{20,})`), 1},
	{KindOpenAIKey, regexp.MustCompile(`\bsk-(?:(?:proj|svcacct|admin)-)?([A-Za-z0-9_-]{20,})`), 1},
	{KindGitHubToken, regexp.MustCompile(`\bgh[po]_([A-Za-z0-9]{36,})\b`), 1},
	{KindGitHubToken, regexp.MustCompile(`\bgithub_pat_([A-Za-z0-9_]{22,})\b`), 1},
	{KindSlackToken, regexp.MustCompile(`\bxox[bp]-([0-9A-Za-z-]{20,})`), 1},
	{KindAWSAccessKey, regexp.MustCompile(`\b(?:AKIA|ASIA)([0-9A-Z]{16})\b`), 1},
	{KindPrivateKey, regexp.MustCompile(`-----BEGIN (?:[A-Z0-9]+ )*PRIVATE KEY-----`), 0},
}

// secretValueRE matches a credential word, a short gap that ends in an
// assignment-like separator, and the value after it (submatch 1).
var secretValueRE = regexp.MustCompile(
	`(?i)(?:token|secret|passw(?:or)?d|pwd|api[_-]?key|토큰|비밀번호|시크릿)` +
		`[\p{L}\p{N}_ .\-"']{0,24}?(?:[:=]|\s|=>|은|는|이|가|을|를)\s*["'` + "`" + `]?` +
		`([A-Za-z0-9+/_.~=-]{16,})`)

var (
	sha40RE = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)
	hexRE   = regexp.MustCompile(`^[0-9a-fA-F]+$`)
	uuidRE  = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

// Scan returns the key-shaped spans in text, ordered by position and never
// overlapping.
func Scan(text string) []Finding {
	var found []Finding
	for _, r := range prefixRules {
		for _, m := range r.re.FindAllStringSubmatchIndex(text, -1) {
			if r.body > 0 {
				body := text[m[2*r.body]:m[2*r.body+1]]
				if !looksRandom(body, 0) {
					continue
				}
				if r.kind == KindAWSAccessKey && strings.HasSuffix(body, "EXAMPLE") {
					continue
				}
			}
			found = append(found, Finding{Kind: r.kind, Start: m[0], End: m[1]})
		}
	}
	for _, m := range secretValueRE.FindAllStringSubmatchIndex(text, -1) {
		v := text[m[2]:m[3]]
		if notASecret(v) || !looksRandom(v, 3.0) {
			continue
		}
		found = append(found, Finding{Kind: KindSecretValue, Start: m[2], End: m[3]})
	}
	return dedupe(found)
}

// notASecret drops values with a well-known non-secret shape: commit SHAs,
// UUIDs and hash digests.
func notASecret(v string) bool {
	v = strings.TrimRight(v, ".")
	if sha40RE.MatchString(v) || uuidRE.MatchString(v) {
		return true
	}
	if hexRE.MatchString(v) && len(v) == 64 {
		return true
	}
	return strings.HasPrefix(v, "sha256:") || strings.HasPrefix(v, "h1:")
}

// looksRandom rejects placeholders and words: the value needs a letter and
// a digit, at least 10 distinct characters, and (when minEntropy > 0) that
// much Shannon entropy per character.
func looksRandom(s string, minEntropy float64) bool {
	var letter, digit bool
	counts := map[rune]int{}
	n := 0
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9':
			digit = true
		case (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z'):
			letter = true
		}
		counts[c]++
		n++
	}
	if !letter || !digit || len(counts) < 10 {
		return false
	}
	if minEntropy <= 0 {
		return true
	}
	var h float64
	for _, k := range counts {
		p := float64(k) / float64(n)
		h -= p * math.Log2(p)
	}
	return h >= minEntropy
}

func dedupe(fs []Finding) []Finding {
	if len(fs) < 2 {
		return fs
	}
	// Earlier start first; for the same start the longer span (and the more
	// specific prefix rule, which comes first in fs) wins.
	sort.SliceStable(fs, func(i, j int) bool {
		if fs[i].Start != fs[j].Start {
			return fs[i].Start < fs[j].Start
		}
		return fs[i].End > fs[j].End
	})
	out := fs[:1]
	for _, f := range fs[1:] {
		if f.Start < out[len(out)-1].End {
			continue
		}
		out = append(out, f)
	}
	return out
}
