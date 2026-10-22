// Package words turns random bits into short, easy-to-say words.
//
// The list is the EFF short wordlist 1 (CC BY 3.0, Electronic Frontier
// Foundation, https://www.eff.org/dice), minus "yo-yo" (the list also
// has "yoyo") so that words can be joined with hyphens. 1295 words, 10.3 bits each.
package words

import (
	"crypto/rand"
	_ "embed"
	"math/big"
	"strings"
)

//go:embed words.txt
var raw string

// List is the word list in its original order.
var List = strings.Fields(raw)

var index = func() map[string]int {
	m := make(map[string]int, len(List))
	for i, w := range List {
		m[w] = i
	}
	return m
}()

// Random returns n words chosen uniformly at random.
func Random(n int) []string {
	out := make([]string, n)
	max := big.NewInt(int64(len(List)))
	for i := range out {
		r, err := rand.Int(rand.Reader, max)
		if err != nil {
			panic(err) // crypto/rand never fails on supported platforms
		}
		out[i] = List[r.Int64()]
	}
	return out
}

// Valid reports whether w is in the list.
func Valid(w string) bool {
	_, ok := index[w]
	return ok
}

// Closest returns the list word nearest to w by edit distance, for
// "did you mean" hints. It returns "" if nothing is within two edits.
func Closest(w string) string {
	best, bestD := "", 3
	for _, c := range List {
		if d := distance(w, c); d < bestD {
			best, bestD = c, d
		}
	}
	return best
}

func distance(a, b string) int {
	// Optimal string alignment: like Levenshtein, but swapping two
	// neighbouring letters, the commonest typo, counts as one edit.
	d := make([][]int, len(a)+1)
	for i := range d {
		d[i] = make([]int, len(b)+1)
		d[i][0] = i
	}
	for j := range d[0] {
		d[0][j] = j
	}
	for i := 1; i <= len(a); i++ {
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			d[i][j] = min(d[i-1][j]+1, d[i][j-1]+1, d[i-1][j-1]+cost)
			if i > 1 && j > 1 && a[i-1] == b[j-2] && a[i-2] == b[j-1] {
				d[i][j] = min(d[i][j], d[i-2][j-2]+1)
			}
		}
	}
	return d[len(a)][len(b)]
}
