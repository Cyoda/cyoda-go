package renderer

import (
	"testing"
)

func TestFindUnsupported_TableDetected(t *testing.T) {
	issues := FindUnsupported([]byte("| col |\n|-----|\n| val |\n"))
	if len(issues) == 0 {
		t.Fatal("pipe table must be flagged")
	}
}

func TestFindUnsupported_NestedListDetected(t *testing.T) {
	issues := FindUnsupported([]byte("- a\n  - nested\n"))
	if len(issues) == 0 {
		t.Fatal("nested list must be flagged")
	}
}

func TestFindUnsupported_NestedNumberedListDetected(t *testing.T) {
	for _, src := range []string{"- a\n  1. nested\n", "- a\n\t2. nested\n", "- a\n  10. nested\n"} {
		if len(FindUnsupported([]byte(src))) == 0 {
			t.Errorf("nested numbered list must be flagged: %q", src)
		}
	}
}

func TestFindUnsupported_HTMLBlockDetected(t *testing.T) {
	issues := FindUnsupported([]byte("<div>x</div>\n"))
	if len(issues) == 0 {
		t.Fatal("HTML block must be flagged")
	}
}

func TestFindUnsupported_CleanContentHasNoIssues(t *testing.T) {
	src := []byte("# Title\n\nParagraph text with **bold** and `code`.\n\n- bullet one\n- bullet two\n\n```\nfenced code\n```\n")
	issues := FindUnsupported(src)
	if len(issues) != 0 {
		t.Errorf("clean content flagged: %+v", issues)
	}
}

func TestFindUnsupported_PipeTableInsideFenceIsExempt(t *testing.T) {
	// Content that LOOKS like a pipe table but is inside a fenced code
	// block must not be flagged — example usage in help docs is legitimate.
	src := []byte("```\n| col |\n|-----|\n| val |\n```\n")
	if issues := FindUnsupported(src); len(issues) != 0 {
		t.Errorf("fenced pipe table flagged: %+v", issues)
	}
}
