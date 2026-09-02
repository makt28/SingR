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

func TestIsRandomPaddingScheme(t *testing.T) {
	cases := map[string]bool{
		"":       false,
		"random": true,
		"RANDOM": true,
		" random ": true,
	}
	for in, want := range cases {
		if got := isRandomPaddingScheme([]string{in}); in != "" && got != want {
			t.Fatalf("%q: got %v want %v", in, got, want)
		}
	}
	if isRandomPaddingScheme([]string{}) {
		t.Fatal("empty list should not be random")
	}
	if isRandomPaddingScheme([]string{"random", "extra"}) {
		t.Fatal("multi-element list should not be random")
	}
}
