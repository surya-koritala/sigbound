package gitobject

import "testing"

func FuzzParseHeader(f *testing.F) {
	for _, seed := range []string{
		"0123456789012345678901234567890123456789 blob 12\n",
		"0123456789012345678901234567890123456789 commit 0\n",
		"deadbeef missing\n",
		"\n",
		"bad tree -1\n",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, line string) {
		h, err := parseHeader(line)
		if err != nil {
			return
		}
		if h.missing {
			if h.oid != "" || h.typ != "" || h.size != 0 || h.missingSpec == "" {
				t.Fatalf("missing header retained fields: %+v", h)
			}
			return
		}
		if !validOID(h.oid) || !validType(h.typ) || h.size < 0 {
			t.Fatalf("accepted invalid header: %+v", h)
		}
	})
}
