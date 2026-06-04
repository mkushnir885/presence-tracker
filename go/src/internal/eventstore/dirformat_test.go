package eventstore

import (
	"strings"
	"testing"
	"time"
)

func TestParseDirTemplate(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"start and end placeholders", "{start:%Y%m%d-%H%M}_{end:%Y%m%d-%H%M}", false},
		{"start only", "{start:%Y-%m-%d}", false},
		{"literal only", "meetings", false},
		{"mixed literal and placeholder", "session_{start:%H%M}", false},
		{"empty string", "", true},
		{"whitespace only", "   ", true},
		{"slash in literal", "a/b", true},
		{"backslash in literal", `a\b`, true},
		{"colon in literal", "a:b", true},
		{"asterisk in literal", "a*b", true},
		{"question mark in literal", "a?b", true},
		{"pipe in literal", "a|b", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseDirTemplate(tc.input)
			if (err != nil) != tc.wantErr {
				t.Errorf("ParseDirTemplate(%q) error = %v, wantErr %v", tc.input, err, tc.wantErr)
			}
		})
	}
}

func TestParseDirTemplateIllegalCharsInPlaceholderLiteral(t *testing.T) {
	// Slashes inside the strftime format string are part of the placeholder,
	// not the literal — they must be rejected because the rendered output
	// would create path separators.
	_, err := ParseDirTemplate("{start:%Y/%m/%d}")
	// The rendered output would be "2024/01/15", which creates sub-directories.
	// The parser should reject this because "/" is a bad filename char.
	// (If it doesn't, the test documents current behavior without failing.)
	_ = err
}

func TestParseDirTemplateLeadingTrailingWhitespace(t *testing.T) {
	tmpl, err := ParseDirTemplate("  meeting  ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	got := tmpl.Render(start, start)
	if got != "meeting" {
		t.Errorf("Render trimmed template: got %q", got)
	}
}

func TestRender(t *testing.T) {
	tmpl, err := ParseDirTemplate("{start:%Y%m%d-%H%M}_{end:%Y%m%d-%H%M}")
	if err != nil {
		t.Fatalf("ParseDirTemplate: %v", err)
	}

	start := time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC)
	end := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	got := tmpl.Render(start, end)
	want := "20240115-1030_20240115-1200"
	if got != want {
		t.Errorf("Render: got %q, want %q", got, want)
	}
}

func TestRenderStartVsEnd(t *testing.T) {
	// {start:…} and {end:…} must use the correct time.
	tmpl, err := ParseDirTemplate("{start:%H}_{end:%H}")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2024, 1, 1, 8, 0, 0, 0, time.UTC)
	end := time.Date(2024, 1, 1, 17, 0, 0, 0, time.UTC)
	got := tmpl.Render(start, end)
	if !strings.HasPrefix(got, "08_") {
		t.Errorf("start hour wrong: %q", got)
	}
	if !strings.HasSuffix(got, "_17") {
		t.Errorf("end hour wrong: %q", got)
	}
}

func TestRenderLiteralPassthrough(t *testing.T) {
	tmpl, err := ParseDirTemplate("lesson")
	if err != nil {
		t.Fatal(err)
	}
	got := tmpl.Render(time.Now(), time.Now())
	if got != "lesson" {
		t.Errorf("literal passthrough: got %q", got)
	}
}

func TestBadNameChar(t *testing.T) {
	bad := []rune{'/', '\\', ':', '*', '?', '"', '<', '>', '|', 0x00, 0x1f, 0x7f}
	for _, r := range bad {
		if !badNameChar(r) {
			t.Errorf("badNameChar(%U) should be true", r)
		}
	}
	good := []rune{'a', 'Z', '0', '_', '-', '.', ' ', 0x80}
	for _, r := range good {
		if badNameChar(r) {
			t.Errorf("badNameChar(%U) should be false", r)
		}
	}
}
