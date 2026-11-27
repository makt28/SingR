package anytls

import (
	"testing"

	"github.com/anytls/sing-anytls/padding"
)

func TestRandomPaddingSchemeAlwaysValid(t *testing.T) {
	for range 2000 {
		raw, md5hex := randomPaddingScheme()
		f := padding.NewPaddingFactory(raw)
		if f == nil {
			t.Fatalf("invalid scheme (NewService would fail):\n%s", raw)
		}
		if f.Md5 != md5hex {
			t.Fatalf("md5 mismatch: got %s want %s", md5hex, f.Md5)
		}
		if f.Stop < 5 || f.Stop > 8 {
			t.Fatalf("stop out of range: %d", f.Stop)
		}
		// every shaped packet index must yield at least one usable size
		for pkt := uint32(1); pkt < f.Stop; pkt++ {
			if len(f.GenerateRecordPayloadSizes(pkt)) == 0 {
				t.Fatalf("pkt %d produced no sizes:\n%s", pkt, raw)
			}
		}
	}
	// show one sample for eyeballing
	raw, _ := randomPaddingScheme()
	t.Logf("sample scheme:\n%s", raw)
}

func TestIsPaddingSentinel(t *testing.T) {
	cases := map[string]bool{
		"random":   true,
		"RANDOM":   true,
		" random ": true,
		"randomx":  false,
		"default":  false,
	}
	for in, want := range cases {
		if got := isPaddingSentinel([]string{in}, randomPaddingSchemeSentinel); got != want {
			t.Fatalf("%q: got %v want %v", in, got, want)
		}
	}
	if isPaddingSentinel([]string{}, randomPaddingSchemeSentinel) {
		t.Fatal("empty list is not the sentinel itself")
	}
	if isPaddingSentinel([]string{"random", "extra"}, randomPaddingSchemeSentinel) {
		t.Fatal("multi-element list should not be a sentinel")
	}
}

// Empty padding_scheme must mean random (since 2026-11), not the public
// default: every install that never set the field relies on this.
func TestResolvePaddingSchemeEmptyIsRandom(t *testing.T) {
	defaultMD5 := padding.NewPaddingFactory(padding.DefaultPaddingScheme).Md5
	for _, in := range [][]string{nil, {}, {"random"}, {" Random "}} {
		raw, source, md5hex := resolvePaddingScheme(in)
		f := padding.NewPaddingFactory(raw)
		if f == nil {
			t.Fatalf("%q: invalid scheme:\n%s", in, raw)
		}
		if source != "randomized" || f.Md5 != md5hex || md5hex == defaultMD5 {
			t.Fatalf("%q: want a randomized scheme, got source=%q md5=%s (default %s)", in, source, md5hex, defaultMD5)
		}
	}
	a, _, _ := resolvePaddingScheme(nil)
	b, _, _ := resolvePaddingScheme(nil)
	if string(a) == string(b) {
		t.Fatalf("two startups produced the same scheme:\n%s", a)
	}
}

func TestResolvePaddingSchemeDefaultSentinel(t *testing.T) {
	for _, in := range [][]string{{"default"}, {"DEFAULT"}} {
		raw, source, md5hex := resolvePaddingScheme(in)
		if string(raw) != string(padding.DefaultPaddingScheme) || source != "public default" {
			t.Fatalf("%q: want the public default, got source=%q:\n%s", in, source, raw)
		}
		if md5hex != padding.NewPaddingFactory(raw).Md5 {
			t.Fatalf("%q: md5 mismatch", in)
		}
	}
}

func TestResolvePaddingSchemeVerbatim(t *testing.T) {
	in := []string{"stop=3", "0=10-20", "1=100-200", "2=300-400"}
	raw, source, md5hex := resolvePaddingScheme(in)
	want := "stop=3\n0=10-20\n1=100-200\n2=300-400"
	if string(raw) != want || source != "from config" {
		t.Fatalf("got source=%q:\n%s", source, raw)
	}
	if md5hex != padding.NewPaddingFactory(raw).Md5 {
		t.Fatal("md5 mismatch")
	}
}
