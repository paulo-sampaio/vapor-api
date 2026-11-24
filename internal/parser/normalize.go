package parser

import (
	"regexp"
	"strings"
)

var (
	reClockSpeed = regexp.MustCompile(`(?i)(?:@|\bat\b)?\s*\d+(?:[.,]\d+)?\s*(?:ghz|mhz)\b`)
	reCoreCount  = regexp.MustCompile(`(?i)\(\s*\d+\s*(?:cpus?|cores?|threads?|vcpus?)\s*\)`)
	reVRAMParens = regexp.MustCompile(`(?i)\(\s*(\d+)\s*(gb|g)\b[^)]*\)`)
	reVRAMSpace  = regexp.MustCompile(`(?i)\b(\d+)\s+(gb)\b`)
	reParens     = regexp.MustCompile(`[()\[\]]`)
	reMultiSpace = regexp.MustCompile(`\s+`)
	reSlugNonAl  = regexp.MustCompile(`[^a-z0-9]+`)

	// Shorthand normalizations
	reCoreISpace    = regexp.MustCompile(`\bcore\s+(i[3579])\s+(\d{3,5}[a-z0-9]*)\b`)
	reBareISpace    = regexp.MustCompile(`\b(i[3579])\s+(\d{3,5}[a-z0-9]*)\b`)
	reCoreUltraDash = regexp.MustCompile(`\bcore\s+ultra\s+([3579])-(\d{3}[a-z]*)\b`)
	reRyzenShort    = regexp.MustCompile(`\br([3579])\s+(\d{4}[a-z0-9]*)\b`)
)

// Multilingual filler phrases ordered longest-first so multi-word phrases
// match cleanly before single-word remnants.
var fillerPhrases = []string{
	"or equivalent",
	"or better",
	"or higher",
	"or above",
	"or faster",
	"or greater",
	"ou equivalente",
	"ou superior",
	"ou melhor",
	"o equivalente",
	"o superior",
	"o mejor",
	"o superiore",
	"oder besser",
	"oder gleichwertig",
	"oder höher",
	"oder hoher",
	"ou équivalent",
	"ou equivalent",
	"ou supérieur",
	"ou superieur",
	"ou mieux",
	"lub lepszy",
	"lub odpowiednik",
	"graphics card",
	"video card",
	"discrete gpu",
	"recommended",
	"equivalent",
	"processor",
	"minimum",
}

// NormalizeHardwareName converts raw hardware strings (from PassMark or Steam)
// into a deterministic canonical lowercase representation while preserving
// model identifiers, variant suffixes, and VRAM tokens (e.g. 6gb, 8gb).
func NormalizeHardwareName(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" {
		return ""
	}

	// Normalize smart quotes and dashes.
	replacer := strings.NewReplacer(
		"–", "-",
		"—", "-",
		"−", "-",
		"™", "",
		"®", "",
		"(tm)", "",
		"(r)", "",
		"’", "'",
		"‘", "'",
		"“", `"`,
		"”", `"`,
	)
	s = replacer.Replace(s)

	// Normalize VRAM tokens inside parentheses like "(6 GB)" -> " 6gb ".
	s = reVRAMParens.ReplaceAllString(s, " ${1}gb ")
	// Normalize spaced VRAM tokens like "6 gb" -> "6gb".
	s = reVRAMSpace.ReplaceAllString(s, "${1}gb")

	// Strip clock speeds and core-count parentheticals.
	s = reCoreCount.ReplaceAllString(s, " ")
	s = reClockSpeed.ReplaceAllString(s, " ")

	// Remove remaining brackets/parentheses and punctuation noise.
	s = reParens.ReplaceAllString(s, " ")
	s = strings.NewReplacer(",", " ", "/", " ", "|", " ", ";", " ", ":", " ").Replace(s)

	// Strip multilingual filler phrases with word boundaries.
	for _, phrase := range fillerPhrases {
		s = stripPhraseWordBounded(s, phrase)
	}

	// Normalize common CPU shorthand variants.
	s = reCoreISpace.ReplaceAllString(s, "core $1-$2")
	s = reBareISpace.ReplaceAllString(s, "$1-$2")
	s = reCoreUltraDash.ReplaceAllString(s, "core ultra $1 $2")
	s = reRyzenShort.ReplaceAllString(s, "ryzen $1 $2")

	// Collapse whitespace and trim trailing/leading hyphens.
	s = reMultiSpace.ReplaceAllString(s, " ")
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "-_")
	return s
}

func stripPhraseWordBounded(s, phrase string) string {
	idx := strings.Index(s, phrase)
	for idx != -1 {
		beforeOK := idx == 0 || isWordSep(s[idx-1])
		end := idx + len(phrase)
		afterOK := end == len(s) || isWordSep(s[end])
		if beforeOK && afterOK {
			s = s[:idx] + " " + s[end:]
			idx = strings.Index(s, phrase)
			continue
		}
		next := strings.Index(s[idx+1:], phrase)
		if next == -1 {
			break
		}
		idx = idx + 1 + next
	}
	return s
}

func isWordSep(b byte) bool {
	return b == ' ' || b == '\t' || b == '-' || b == ',' || b == '.'
}

// Slugify converts a normalized hardware name into an ASCII lowercase slug.
func Slugify(norm string) string {
	s := strings.ToLower(norm)
	s = reSlugNonAl.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}

// MakeHardwareID builds a deterministic slug primary key: "<kind>:<slug>".
func MakeHardwareID(kind, norm string) string {
	return kind + ":" + Slugify(norm)
}

// DetectVendor infers the hardware vendor from a hardware display/normalized name.
func DetectVendor(name string) string {
	low := strings.ToLower(name)
	switch {
	case strings.Contains(low, "nvidia") ||
		strings.Contains(low, "geforce") ||
		strings.Contains(low, "gtx") ||
		strings.Contains(low, "rtx") ||
		strings.Contains(low, "quadro") ||
		strings.Contains(low, "titan"):
		return "NVIDIA"
	case strings.Contains(low, "amd") ||
		strings.Contains(low, "ryzen") ||
		strings.Contains(low, "radeon") ||
		strings.Contains(low, "athlon") ||
		strings.Contains(low, "threadripper") ||
		strings.Contains(low, "epyc") ||
		strings.Contains(low, "fx-") ||
		strings.Contains(low, "phenom") ||
		strings.Contains(low, "steam deck"):
		return "AMD"
	case strings.Contains(low, "intel") ||
		strings.Contains(low, "core i") ||
		strings.Contains(low, "core 2") ||
		strings.Contains(low, "core ultra") ||
		strings.Contains(low, "pentium") ||
		strings.Contains(low, "celeron") ||
		strings.Contains(low, "xeon") ||
		strings.Contains(low, "arc "):
		return "Intel"
	case strings.Contains(low, "apple") ||
		strings.HasPrefix(low, "m1") ||
		strings.HasPrefix(low, "m2") ||
		strings.HasPrefix(low, "m3") ||
		strings.HasPrefix(low, "m4"):
		return "Apple"
	default:
		return "Other"
	}
}
