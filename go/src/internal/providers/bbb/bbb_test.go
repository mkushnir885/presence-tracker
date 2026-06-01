package bbb

import "testing"

func TestParseMeetingID(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{"bare id", "my-class-2026-05-01", "my-class-2026-05-01", false},
		{"bare id with whitespace", "  abc-def  ", "abc-def", false},
		{"join url with meetingID query", "https://bbb.example.com/bigbluebutton/api/join?meetingID=abc123&fullName=John&checksum=deadbeef", "abc123", false},
		{"meetingID query takes priority over greenlight path", "https://bbb.example.com/b/path-id/?meetingID=query-id", "query-id", false},
		{"greenlight /b/ url", "https://bbb.example.com/b/abc-def-ghi-jkl", "abc-def-ghi-jkl", false},
		{"greenlight /b/ url trailing slash", "https://bbb.example.com/b/abc-def-ghi-jkl/", "abc-def-ghi-jkl", false},
		{"greenlight /rooms/ url", "https://bbb.example.com/rooms/abc-def-ghi", "abc-def-ghi", false},
		{"greenlight /rooms/ url with /join", "https://bbb.example.com/rooms/abc-def-ghi/join", "abc-def-ghi", false},
		{"greenlight nested under prefix", "https://bbb.example.com/gl/b/room-id", "room-id", false},
		{"empty input", "", "", true},
		{"whitespace only", "   ", "", true},
		{"unrecognised path", "https://bbb.example.com/somewhere/else", "", true},
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
