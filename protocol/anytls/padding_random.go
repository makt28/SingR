package anytls

import (
	"crypto/md5"
	"crypto/rand"
	"fmt"
	"math/big"
	"strings"

	"github.com/anytls/sing-anytls/padding"
)

// padding_scheme resolution (SingR divergence from upstream sing-box, where
// an empty list means the public padding.DefaultPaddingScheme):
//
//   - empty, or exactly ["random"] → a fresh scheme synthesized at process
//     startup (randomPaddingScheme). Empty means random since 2026-11, so
//     every install sheds the public default fingerprint without opting in.
//   - exactly ["default"] → the public padding.DefaultPaddingScheme (the
//     pre-2026-11 meaning of empty; the explicit way back).
//   - any other list → used verbatim, lines joined with "\n".
//
// Sentinels are single-entry and case-insensitive. The scheme is
// authoritative server-side (clients auto-sync via cmdUpdatePaddingScheme),
// so only the server needs it. It is baked into the anytls.Service at
// construction and is not hot-reloaded, so a random scheme changes only on
// restart.
const (
	randomPaddingSchemeSentinel  = "random"
	defaultPaddingSchemeSentinel = "default"
)

// resolvePaddingScheme returns the scheme to hand to anytls.NewService, a
// short description of where it came from (for the startup log) and its md5
// (the value clients key on).
func resolvePaddingScheme(configured []string) (scheme []byte, source string, md5hex string) {
	switch {
	case len(configured) == 0 || isPaddingSentinel(configured, randomPaddingSchemeSentinel):
		scheme, md5hex = randomPaddingScheme()
		return scheme, "randomized", md5hex
	case isPaddingSentinel(configured, defaultPaddingSchemeSentinel):
		scheme = padding.DefaultPaddingScheme
		source = "public default"
	default:
		scheme = []byte(strings.Join(configured, "\n"))
		source = "from config"
	}
	return scheme, source, fmt.Sprintf("%x", md5.Sum(scheme))
}

// isPaddingSentinel reports whether the configured padding_scheme is exactly
// the given single-entry sentinel (case-insensitive, surrounding space ignored).
func isPaddingSentinel(scheme []string, sentinel string) bool {
	return len(scheme) == 1 && strings.EqualFold(strings.TrimSpace(scheme[0]), sentinel)
}

// randomPaddingScheme builds a fresh, valid padding scheme that stays inside
// the "believable early HTTPS record" envelope while differing from the
// public default. It shapes the first stop-1 records of every session:
//
//   - stop:      5..8 records shaped.
//   - record 1:  a small initial record, lower bound 24..64, upper bound
//     lower+40..400 bytes.
//   - records 2+: 1..3 body segments joined by the "c" continue-marker,
//     each lower bound 200..700 and upper bound lower+100..500 (so a
//     segment spans 200..1200 bytes), mirroring the default's
//     multi-segment splitting.
//
// There is no "0=" line, so once a client has synced this scheme its auth
// packet carries no padding (the default pads it by a fixed 30 bytes).
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
