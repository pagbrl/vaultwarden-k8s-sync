package vaultwarden

import (
	"reflect"
	"testing"
)

func TestParseNotes(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want map[string]string
	}{
		{
			name: "empty",
			in:   "",
			want: map[string]string{},
		},
		{
			name: "plain key=value",
			in:   "FOO=bar",
			want: map[string]string{"FOO": "bar"},
		},
		{
			name: "export prefix is stripped",
			in:   "export FOO=bar",
			want: map[string]string{"FOO": "bar"},
		},
		{
			name: "mixed export and plain",
			in:   "export FOO=bar\nBAZ=qux",
			want: map[string]string{"FOO": "bar", "BAZ": "qux"},
		},
		{
			name: "comments and blank lines skipped",
			in:   "# a comment\n\nFOO=bar\n   \n# another\nBAZ=qux\n",
			want: map[string]string{"FOO": "bar", "BAZ": "qux"},
		},
		{
			name: "indented comment skipped",
			in:   "   # indented comment\nFOO=bar",
			want: map[string]string{"FOO": "bar"},
		},
		{
			name: "equals sign in value preserved",
			in:   "DB_URL=postgres://u:p@host:5432/db?sslmode=require",
			want: map[string]string{"DB_URL": "postgres://u:p@host:5432/db?sslmode=require"},
		},
		{
			name: "base64 value with padding equals",
			in:   "TOKEN=YWJjZGVmZ2g=",
			want: map[string]string{"TOKEN": "YWJjZGVmZ2g="},
		},
		{
			name: "CRLF line endings",
			in:   "export FOO=bar\r\nBAZ=qux\r\n",
			want: map[string]string{"FOO": "bar", "BAZ": "qux"},
		},
		{
			name: "whitespace around key trimmed",
			in:   "  FOO =bar",
			want: map[string]string{"FOO": "bar"},
		},
		{
			name: "value whitespace preserved verbatim",
			in:   "GREETING=hello world ",
			want: map[string]string{"GREETING": "hello world "},
		},
		{
			name: "empty value allowed",
			in:   "EMPTY=",
			want: map[string]string{"EMPTY": ""},
		},
		{
			name: "line without equals ignored",
			in:   "not a pair\nFOO=bar",
			want: map[string]string{"FOO": "bar"},
		},
		{
			name: "empty key ignored",
			in:   "=orphan\nFOO=bar",
			want: map[string]string{"FOO": "bar"},
		},
		{
			name: "later key overrides earlier",
			in:   "FOO=first\nFOO=second",
			want: map[string]string{"FOO": "second"},
		},
		{
			name: "export word not treated as prefix without space",
			in:   "exportFOO=bar",
			want: map[string]string{"exportFOO": "bar"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseNotes(tt.in)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ParseNotes(%q) = %#v, want %#v", tt.in, got, tt.want)
			}
		})
	}
}
