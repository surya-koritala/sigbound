package cell

import (
	"reflect"
	"testing"
)

func TestWriteSetBasics(t *testing.T) {
	w := NewWriteSet("b", "a", "a")
	if w.Len() != 2 {
		t.Fatalf("Len = %d, want 2 (dedup)", w.Len())
	}
	if !w.Contains("a") || w.Contains("z") {
		t.Fatal("Contains wrong")
	}
	if got := w.Paths(); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("Paths = %v, want sorted [a b]", got)
	}
	w.Add("c")
	if w.Len() != 3 {
		t.Fatalf("after Add Len = %d, want 3", w.Len())
	}
}

func TestWriteSetOverlaps(t *testing.T) {
	cases := []struct {
		name string
		a, b []string
		want bool
	}{
		{"share one", []string{"x", "y"}, []string{"y", "z"}, true},
		{"disjoint", []string{"a", "b"}, []string{"c", "d"}, false},
		{"empty vs nonempty", nil, []string{"a"}, false},
		{"identical", []string{"a"}, []string{"a"}, true},
		{"subset", []string{"a"}, []string{"a", "b", "c"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := NewWriteSet(tc.a...)
			b := NewWriteSet(tc.b...)
			if got := a.Overlaps(b); got != tc.want {
				t.Fatalf("Overlaps = %v, want %v", got, tc.want)
			}
			// symmetric
			if got := b.Overlaps(a); got != tc.want {
				t.Fatalf("Overlaps (rev) = %v, want %v", got, tc.want)
			}
		})
	}
}
