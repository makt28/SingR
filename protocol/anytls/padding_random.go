package anytls

import (
	"crypto/md5"
	"crypto/rand"
	"fmt"
	"math/big"
	"strings"
)

// randomPaddingSchemeSentinel is the opt-in marker: setting the anytls
// inbound's padding_scheme to exactly ["random"] makes the server
// synthesize a fresh, valid padding scheme at process startup instead of
// shipping the public padding.DefaultPaddingScheme. The scheme is
// authoritative server-side (clients auto-sync via cmdUpdatePaddingScheme),
// so only the server needs it. It is baked into the anytls.Service at
// construction and is not hot-reloaded, matching the per-startup semantics.
const randomPaddingSchemeSentinel = "random"

// isRandomPaddingScheme reports whether the configured padding_scheme is the
// opt-in random sentinel (a single "random" entry, case-insensitive).
func isRandomPaddingScheme[T ~[]string](scheme T) bool {
	return len(scheme) == 1 && strings.EqualFold(strings.TrimSpace(scheme[0]), randomPaddingSchemeSentinel)
}

// randomPaddingScheme builds a fresh, valid padding scheme that stays inside
// the "believable early HTTPS record" envelope while differing from the
// public default. It shapes the first stop-1 records of every session:
//
//   - stop:      5..8 records shaped.
//   - record 1:  a small initial record (24..64 .. up to 400 bytes).
//   - records 2+: 1..3 body segments joined by the "c" continue-marker,
//     each 300..1200 bytes, mirroring the default's multi-segment splitting.
//
// It returns the scheme and its md5 (the value clients key on) for logging.
func randomPaddingScheme() (scheme []byte, md5hex string) {
	stop := randInt(5, 8)

	var b strings.Builder
	fmt.Fprintf(&b, "stop=%d\n", stop)
	for pkt := 1; pkt < stop; pkt++ {
		if pkt == 1 {
			lo := randInt(24, 64)
			hi := randInt(lo+40, 400)
			fmt.Fprintf(&b, "1=%d-%d\n", lo, hi)
			continue
		}
		segs := randInt(1, 3)
		parts := make([]string, 0, segs)
		for range segs {
			lo := randInt(200, 700)
			hi := lo + randInt(100, 500)
			parts = append(parts, fmt.Sprintf("%d-%d", lo, hi))
		}
		fmt.Fprintf(&b, "%d=%s\n", pkt, strings.Join(parts, ",c,"))
	}

	raw := []byte(strings.TrimRight(b.String(), "\n"))
	sum := md5.Sum(raw)
	return raw, fmt.Sprintf("%x", sum)
}

// randInt returns a uniform random int in [min, max] inclusive, drawn from
// crypto/rand. On the (practically impossible) rand failure it degrades to
// min, which is always a valid scheme value.
func randInt(min, max int) int {
	if max <= min {
		return min
	}
	n, err := rand.Int(rand.Reader, big.NewInt(int64(max-min+1)))
	if err != nil {
		return min
	}
	return min + int(n.Int64())
}
