// Package rqlitebackend guards the release acceptance record of the rqlite +
// etcd v3 compatibility backend (Issue #619, epic #592).
//
// The record is a human document, but its honesty rules are executable: a
// record that drops a completion criterion, drops one of the four non-goals,
// loses a pinned version, or claims "met" without an evidence pointer must fail
// this test. All helper code lives in this test file so the package adds no
// shippable code and no coverage obligation.
package rqlitebackend

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const recordPath = "../../hack/rqlite-backend/RELEASE-RECORD.md"

var (
	requiredHeadings = []string{
		"## 2. Exact versions",
		"## 3. Non-goals (binding)",
		"## 4. Completion criteria and gate status",
		"## 5. Executed test scope",
		"## 6. Unexecuted scenarios",
		"## 7. Limitations",
		"## 8. Release gate",
	}
	// The four non-goals from Issue #619, identified by a stable phrase each.
	nonGoalTokens = []string{"rqlite/Raft", "S3", "分片", "使用成熟 rqlite"}
	validStatuses = map[string]bool{"met": true, "not-met": true, "not-executed": true}
	criterionID   = regexp.MustCompile(`^C[1-5]$`)
	sha256Re      = regexp.MustCompile(`\b[0-9a-f]{64}\b`)
	linkRe        = regexp.MustCompile(`\]\(([^)]+)\)`)
)

// section returns the body of a level-2 heading, up to the next level-2
// heading or the end of the document.
func section(doc, heading string) (string, error) {
	idx := strings.Index(doc, heading)
	if idx < 0 {
		return "", fmt.Errorf("missing heading %q", heading)
	}
	rest := doc[idx+len(heading):]
	if end := strings.Index(rest, "\n## "); end >= 0 {
		rest = rest[:end]
	}
	return rest, nil
}

// tableRows returns the cells of every Markdown table row in a section,
// including the header and separator rows.
func tableRows(body string) [][]string {
	var rows [][]string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		rows = append(rows, cells)
	}
	return rows
}

// bullets returns the top-level list items of a section.
func bullets(body string) []string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "- ") {
			out = append(out, line)
		}
	}
	return out
}

// parseRecord validates every honesty rule the Issue's criterion 5 requires.
// It returns whether every completion criterion is currently met.
func parseRecord(doc string) (allMet bool, err error) {
	for _, heading := range requiredHeadings {
		if _, err := section(doc, heading); err != nil {
			return false, err
		}
	}
	if err := checkVersions(doc); err != nil {
		return false, err
	}
	if err := checkNonGoals(doc); err != nil {
		return false, err
	}
	allMet, err = checkCriteria(doc)
	if err != nil {
		return false, err
	}
	if err := checkMinBullets(doc, "## 6. Unexecuted scenarios", 4, "unexecuted scenario"); err != nil {
		return false, err
	}
	if err := checkMinBullets(doc, "## 7. Limitations", 3, "limitation"); err != nil {
		return false, err
	}
	gate, err := section(doc, "## 8. Release gate")
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(gate) == "" {
		return false, fmt.Errorf("release gate section is empty")
	}
	if !allMet && !strings.Contains(strings.ToLower(doc), "not released") {
		return false, fmt.Errorf("a criterion is unmet but the record does not state the release is not released")
	}
	return allMet, nil
}

// checkVersions requires a pinned version for each component that decides
// compatibility, and a SHA-256 for the rqlite build.
func checkVersions(doc string) error {
	body, err := section(doc, "## 2. Exact versions")
	if err != nil {
		return err
	}
	want := []string{"Kubernetes", "etcd", "rqlite", "Go"}
	found := map[string]bool{}
	for _, row := range tableRows(body) {
		if len(row) < 2 || strings.HasPrefix(row[0], "-") || row[0] == "Component" {
			continue
		}
		for _, label := range want {
			if !strings.Contains(row[0], label) {
				continue
			}
			if strings.TrimSpace(row[1]) == "" {
				return fmt.Errorf("version row %q has no version", row[0])
			}
			found[label] = true
		}
		if strings.Contains(row[0], "rqlite") && !sha256Re.MatchString(strings.Join(row[2:], " ")) {
			return fmt.Errorf("rqlite row has no sha256 digest")
		}
	}
	for _, label := range want {
		if !found[label] {
			return fmt.Errorf("version table is missing a %q row", label)
		}
	}
	return nil
}

// checkNonGoals requires exactly the four non-goals from Issue #619.
func checkNonGoals(doc string) error {
	body, err := section(doc, "## 3. Non-goals (binding)")
	if err != nil {
		return err
	}
	items := bullets(body)
	if len(items) != 4 {
		return fmt.Errorf("non-goals section has %d bullets, want the 4 from Issue #619", len(items))
	}
	joined := strings.Join(items, "\n")
	for _, token := range nonGoalTokens {
		if !strings.Contains(joined, token) {
			return fmt.Errorf("non-goals section is missing the %q non-goal", token)
		}
	}
	return nil
}

// checkCriteria requires one row per completion criterion, a known status, and
// evidence for anything claimed met — the "a process starting is not completion"
// rule.
func checkCriteria(doc string) (allMet bool, err error) {
	body, err := section(doc, "## 4. Completion criteria and gate status")
	if err != nil {
		return false, err
	}
	seen := map[string]bool{}
	allMet = true
	for _, row := range tableRows(body) {
		if len(row) < 4 || !criterionID.MatchString(row[0]) {
			continue
		}
		id, status := row[0], row[2]
		evidence := strings.Join(row[3:], " ")
		if !validStatuses[status] {
			return false, fmt.Errorf("criterion %s has status %q, want met|not-met|not-executed", id, status)
		}
		if strings.TrimSpace(row[1]) == "" {
			return false, fmt.Errorf("criterion %s has no statement", id)
		}
		if status == "met" && (strings.TrimSpace(evidence) == "" || evidence == "-") {
			return false, fmt.Errorf("criterion %s is marked met without an evidence pointer", id)
		}
		if strings.TrimSpace(evidence) == "" {
			return false, fmt.Errorf("criterion %s has no evidence/blocker", id)
		}
		seen[id] = true
		if status != "met" {
			allMet = false
		}
	}
	for i := 1; i <= 5; i++ {
		id := fmt.Sprintf("C%d", i)
		if !seen[id] {
			return false, fmt.Errorf("completion criteria table is missing %s", id)
		}
	}
	return allMet, nil
}

// checkMinBullets requires a named section to carry at least min list items.
func checkMinBullets(doc, heading string, min int, what string) error {
	body, err := section(doc, heading)
	if err != nil {
		return err
	}
	if n := len(bullets(body)); n < min {
		return fmt.Errorf("%s has %d bullets, want at least %d %ss", heading, n, min, what)
	}
	return nil
}

func loadRecord(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatalf("read release record: %v", err)
	}
	return string(b)
}

// TestReleaseRecordSatisfiesContract is the gate: the shipped record must obey
// every rule above.
func TestReleaseRecordSatisfiesContract(t *testing.T) {
	if _, err := parseRecord(loadRecord(t)); err != nil {
		t.Fatalf("release record violates the #619 contract: %v", err)
	}
}

// TestReleaseRecordRelativeLinksResolve rejects a record whose internal links
// point at files or directories that do not exist.
func TestReleaseRecordRelativeLinksResolve(t *testing.T) {
	doc := loadRecord(t)
	base := filepath.Dir(recordPath)
	checked := 0
	for _, m := range linkRe.FindAllStringSubmatch(doc, -1) {
		target := m[1]
		if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") ||
			strings.HasPrefix(target, "#") || strings.HasPrefix(target, "mailto:") {
			continue
		}
		path := target
		if i := strings.IndexByte(path, '#'); i >= 0 {
			path = path[:i]
		}
		if path == "" {
			continue
		}
		checked++
		if _, err := os.Stat(filepath.Join(base, path)); err != nil {
			t.Errorf("release record link %q: %v", target, err)
		}
	}
	if checked == 0 {
		t.Fatal("release record has no relative links to check")
	}
}

// TestRecordContractRejectsIncompleteRecord proves the contract fails on the
// tampering it is meant to catch, so a green run is not a vacuous one.
func TestRecordContractRejectsIncompleteRecord(t *testing.T) {
	good := loadRecord(t)
	if _, err := parseRecord(good); err != nil {
		t.Fatalf("baseline record should satisfy the contract: %v", err)
	}
	cases := []struct {
		name string
		doc  string
	}{
		{"missing criterion row", strings.Replace(good, "| C4 |", "| X4 |", 1)},
		{"met without evidence", rewriteCriterionEvidence(good, "C5", "met", "-")},
		{"missing non-goal", strings.Replace(good, "- 不引入强制 S3，不把 SQLite 文件复制当成数据库一致性协议。\n", "", 1)},
		{"missing version row", strings.Replace(good, "| rqlite |", "| sqlite |", 1)},
		{"empty unexecuted scenarios", dropSectionBody(good, "## 6. Unexecuted scenarios")},
		{"unknown status", strings.Replace(good, "| not-met |", "| done |", 1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.doc == good {
				t.Fatalf("test case %q did not change the record", tc.name)
			}
			if _, err := parseRecord(tc.doc); err == nil {
				t.Fatalf("contract accepted an invalid record: %s", tc.name)
			}
		})
	}
}

// rewriteCriterionEvidence replaces the status and evidence cells of one
// completion-criteria row, so a negative case can tamper with exactly one field.
func rewriteCriterionEvidence(doc, id, status, evidence string) string {
	re := regexp.MustCompile(`(?m)^\| ` + id + ` \|(.*)\| (?:met|not-met|not-executed) \|.*\|$`)
	return re.ReplaceAllString(doc, "| "+id+" |$1| "+status+" | "+evidence+" |")
}

// dropSectionBody removes the body of a section, leaving its heading and the
// following headings intact.
func dropSectionBody(doc, heading string) string {
	idx := strings.Index(doc, heading)
	if idx < 0 {
		return doc
	}
	rest := doc[idx+len(heading):]
	end := strings.Index(rest, "\n## ")
	if end < 0 {
		return doc[:idx+len(heading)]
	}
	return doc[:idx+len(heading)] + rest[end:]
}
