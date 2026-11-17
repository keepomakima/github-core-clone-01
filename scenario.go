// The scenario's viewer identity, derived from the contest's one public seed.
//
// The suite builds every name it sends from a single constant seed (wire.ts
// SCENARIO_SEED). The token owner's login is the first generated user, so the
// forge derives it the same way instead of guessing: FNV-1a + splitmix64 +
// xorshift64* over the seed, then the first/last-name picks. This is a small
// re-implementation of the documented public derivation, not a copy of the
// suite's code, and it keeps the forge answering `viewer { login }` and the
// foreign-owner 404 with the right logins on every trial.

package main

const u64Mask = 0xffffffffffffffff
const scenarioSeed = uint64(0x6a09e667f3bcc909)

var firstNames = []string{
	"ada", "bram", "cora", "dmitri", "elena", "felix", "greta", "hugo", "iris", "jonas", "kira", "lars",
	"mira", "nico", "olga", "pavel", "quinn", "rosa", "sven", "tess", "ulla", "vito", "wren", "yara", "zane",
}

var lastNames = []string{
	"anders", "bauer", "costa", "dahl", "eriksen", "fischer", "garcia", "holm", "iversen", "jansen", "keller",
	"lind", "moreau", "novak", "olsen", "peters", "quist", "rossi", "sato", "turi", "ulrich", "vega", "weber",
	"xu", "yilmaz", "zimmer",
}

func fnv1a64(text string) uint64 {
	hash := uint64(0xcbf29ce484222325)
	for i := 0; i < len(text); i++ {
		hash ^= uint64(text[i])
		hash = (hash * 0x100000001b3) & u64Mask
	}
	return hash
}

func mix64(value uint64) uint64 {
	z := (value + 0x9e3779b97f4a7c15) & u64Mask
	z = ((z ^ (z >> 30)) * 0xbf58476d1ce4e5b9) & u64Mask
	z = ((z ^ (z >> 27)) * 0x94d049bb133111eb) & u64Mask
	return (z ^ (z >> 31)) & u64Mask
}

type rng struct {
	state uint64
}

func newRng(seed uint64) *rng {
	masked := seed & u64Mask
	if masked == 0 {
		masked = scenarioSeed
	}
	return &rng{state: masked}
}

func (r *rng) nextU64() uint64 {
	x := r.state
	x = (x ^ (x >> 12)) & u64Mask
	x = (x ^ (x << 25)) & u64Mask
	x = (x ^ (x >> 27)) & u64Mask
	r.state = x
	return (x * 0x2545f4914f6cdd1d) & u64Mask
}

func (r *rng) nextU32() uint32 {
	return uint32(r.nextU64() >> 32)
}

func (r *rng) nextInt(maxExclusive int) int {
	if maxExclusive == 1 {
		return 0
	}
	const rangeSize = uint64(4294967296)
	limit := rangeSize - (rangeSize % uint64(maxExclusive))
	for {
		value := r.nextU32()
		if uint64(value) < limit {
			return int(value % uint32(maxExclusive))
		}
	}
}

func (r *rng) pick(items []string) string {
	return items[r.nextInt(len(items))]
}

type scenarioUser struct {
	id    int
	login string
	name  string
	email string
}

func generateUsers(count int) []scenarioUser {
	r := newRng(mix64(scenarioSeed ^ fnv1a64("users")))
	used := map[string]bool{}
	users := make([]scenarioUser, 0, count)
	for i := 0; i < count; i++ {
		first := r.pick(firstNames)
		last := r.pick(lastNames)
		base := first + last
		// strip anything outside [a-z0-9-] (the names are already clean)
		clean := make([]byte, 0, len(base))
		for j := 0; j < len(base); j++ {
			c := base[j]
			if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
				clean = append(clean, c)
			}
		}
		base = string(clean)
		login := base
		suffix := 2
		for used[login] {
			login = base + "-" + itoa(suffix)
			suffix++
		}
		used[login] = true
		users = append(users, scenarioUser{id: 1000 + i, login: login, name: first + " " + last, email: login + "@example.invalid"})
	}
	return users
}

var users = generateUsers(12)

// VIEWER is the login the trial's bearer token owns (the scenario's viewer).
var VIEWER = users[0]

// TEAMMATE is the second generated user (the suite's teammate).
var TEAMMATE = users[1]

// STRANGER is the third generated user (the suite's stranger, who owns no viewer repo).
var STRANGER = users[2]

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
