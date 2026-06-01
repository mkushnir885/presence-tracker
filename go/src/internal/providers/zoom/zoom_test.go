package zoom

import "testing"

func TestParseMeetingID(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{"bare numeric id", "1234567890", "1234567890", false},
		{"bare id with whitespace", "  1234567890  ", "1234567890", false},
		{"bare id with embedded spaces", "123 4567 890", "1234567890", false},
		{"join url", "https://zoom.us/j/1234567890", "1234567890", false},
		{"join url with region subdomain", "https://us02web.zoom.us/j/9876543210", "9876543210", false},
		{"join url with passcode", "https://zoom.us/j/1234567890?pwd=abcdef", "1234567890", false},
		{"signed-in join url", "https://zoom.us/s/1234567890", "1234567890", false},
		{"webinar join url", "https://zoom.us/w/1234567890", "1234567890", false},
		{"web client join url", "https://us02web.zoom.us/wc/join/1234567890", "1234567890", false},
		{"join url trailing slash", "https://zoom.us/j/1234567890/", "1234567890", false},
		{"empty input", "", "", true},
		{"whitespace only", "   ", "", true},
		{"personal room url", "https://zoom.us/my/jdoe", "", true},
		{"unrecognised path", "https://zoom.us/somewhere/else", "", true},
		{"malformed url", "http://[::1", "", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := (&Adapter{}).ParseMeetingID(tc.input)
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
