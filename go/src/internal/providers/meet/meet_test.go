package meet

import "testing"

func TestParseMeetingID(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{"bare meeting code", "abc-defg-hij", "abc-defg-hij", false},
		{"bare code with whitespace", "  abc-defg-hij  ", "abc-defg-hij", false},
		{"spaces resource name", "spaces/jQCFfuBOdN5z", "spaces/jQCFfuBOdN5z", false},
		{"meet url", "https://meet.google.com/abc-defg-hij", "abc-defg-hij", false},
		{"meet url with query", "https://meet.google.com/abc-defg-hij?authuser=0", "abc-defg-hij", false},
		{"meet url trailing slash", "https://meet.google.com/abc-defg-hij/", "abc-defg-hij", false},
		{"empty input", "", "", true},
		{"whitespace only", "   ", "", true},
		{"lookup url", "https://meet.google.com/lookup/abcdefghij", "", true},
		{"new meeting url", "https://meet.google.com/new", "", true},
		{"malformed url", "http://[::1", "", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseMeetingID(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
