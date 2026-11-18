package state

import (
	"os"
	"strings"
	"testing"
)

// TestProtocolPrintsTheTable keeps PROTOCOL.md §5.1 and Table the same text.
func TestProtocolPrintsTheTable(t *testing.T) {
	doc, err := os.ReadFile("../PROTOCOL.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, subject := range []string{sLine, sTrain} {
		if !strings.Contains(string(doc), Markdown(subject)) {
			t.Errorf("PROTOCOL.md §5.1 does not carry the %s table as Table prints it; see TestPrintTable", subject)
		}
	}
}

// TestPrintTable writes the tables of PROTOCOL.md §5.1 when SHUNTER_TABLE
// names a file: SHUNTER_TABLE=/tmp/table.md go test ./state -run PrintTable
func TestPrintTable(t *testing.T) {
	path := os.Getenv("SHUNTER_TABLE")
	if path == "" {
		t.Skip("SHUNTER_TABLE not set")
	}
	out := "**Transitions of a line**\n\n" + Markdown(sLine) + "\n**Transitions of a train**\n\n" + Markdown(sTrain)
	if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
		t.Fatal(err)
	}
}
