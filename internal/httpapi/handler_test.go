package httpapi

import (
	"reflect"
	"testing"
)

func TestBuildSourceCandidates(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		rawExt string
		want   []sourceCandidate
	}{
		{
			name:   "single extension",
			input:  "test/13.jpg",
			rawExt: ".jpg",
			want: []sourceCandidate{{
				relative:    "test/13.jpg",
				cacheSuffix: "",
			}},
		},
		{
			name:   "double extension to png",
			input:  "test/13.jpg.png",
			rawExt: ".png",
			want: []sourceCandidate{
				{relative: "test/13.jpg.png", cacheSuffix: ""},
				{relative: "test/13.jpg", cacheSuffix: ".png"},
			},
		},
		{
			name:   "uppercase avif",
			input:  "foo/BAR.PNG.AVIF",
			rawExt: ".AVIF",
			want: []sourceCandidate{
				{relative: "foo/BAR.PNG.AVIF", cacheSuffix: ""},
				{relative: "foo/BAR.PNG", cacheSuffix: ".avif"},
			},
		},
		{
			name:   "unsupported base",
			input:  "foo.gif.webp",
			rawExt: ".webp",
			want: []sourceCandidate{{
				relative:    "foo.gif.webp",
				cacheSuffix: "",
			}},
		},
		{
			name:   "no extension",
			input:  "img/item/13",
			rawExt: "",
			want: []sourceCandidate{{
				relative:    "img/item/13",
				cacheSuffix: "",
			}},
		},
	}

	for i := range tests {
		caseData := tests[i]
		t.Run(caseData.name, func(t *testing.T) {
			got := buildSourceCandidates(caseData.input, caseData.rawExt)
			if !reflect.DeepEqual(got, caseData.want) {
				t.Fatalf("buildSourceCandidates(%q, %q) = %#v, want %#v", caseData.input, caseData.rawExt, got, caseData.want)
			}
		})
	}
}

func TestParseGeometry(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		width     int
		height    int
		expectErr bool
	}{
		{
			name:   "both provided",
			input:  "200x300",
			width:  200,
			height: 300,
		},
		{
			name:   "missing height",
			input:  "120x",
			width:  120,
			height: 0,
		},
		{
			name:   "missing width",
			input:  "x480",
			width:  0,
			height: 480,
		},
		{
			name:      "invalid width",
			input:     "axb",
			expectErr: true,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			w, h, err := parseGeometry(tc.input)
			if tc.expectErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if w != tc.width || h != tc.height {
				t.Fatalf("parseGeometry(%q) = (%d,%d), want (%d,%d)", tc.input, w, h, tc.width, tc.height)
			}
		})
	}
}
