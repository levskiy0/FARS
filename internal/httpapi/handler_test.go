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
