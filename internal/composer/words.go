package composer

import (
	"fmt"
	"slices"
	"strings"
	"unicode"
)

// piece is one numbered part of the owner's text: the bytes [start, end) of the text,
// trimmed, with the punctuation that ended it.
type piece struct{ start, end int }

// segment cuts the owner's text into pieces at line breaks, sentence ends (".", "!" or
// "?" followed by whitespace or the end), ";" and ",". It never cuts at words, so
// "Swiggy and Zomato" stays in one piece. Pieces with no letter or digit are dropped.
func segment(text string) []piece {
	var out []piece
	add := func(start, end int) {
		for start < end && unicode.IsSpace(rune(text[start])) {
			start++
		}
		for end > start && unicode.IsSpace(rune(text[end-1])) {
			end--
		}
		if strings.ContainsFunc(text[start:end], func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) {
			out = append(out, piece{start, end})
		}
	}
	start := 0
	for i := range len(text) {
		switch c := text[i]; {
		case c == '\n':
			add(start, i)
			start = i + 1
		case c == ',' || c == ';',
			(c == '.' || c == '!' || c == '?') && (i+1 == len(text) || unicode.IsSpace(rune(text[i+1]))):
			add(start, i+1)
			start = i + 1
		}
	}
	add(start, len(text))
	return out
}

// numbered is the text with each piece marked [1], [2], ... where it starts.
func numbered(text string, pieces []piece) string {
	var b strings.Builder
	at := 0
	for i, p := range pieces {
		b.WriteString(text[at:p.start])
		fmt.Fprintf(&b, "[%d] ", i+1)
		at = p.start
	}
	b.WriteString(text[at:])
	return b.String()
}

// wording is the owner's text a compose call numbered, and for a re-optimize the rule's
// first wording, which the new words are added under.
type wording struct {
	text   string
	pieces []piece
	prior  string
}

func newWording(text, prior string) *wording {
	return &wording{text: text, pieces: segment(text), prior: prior}
}

// spans is the owner's exact words for the chosen piece numbers, one slice of the text per
// run of consecutive pieces. Numbers out of range or repeated are ignored.
func (w *wording) spans(nums []int) []string {
	var ok []int
	for _, n := range nums {
		if n >= 1 && n <= len(w.pieces) && !slices.Contains(ok, n) {
			ok = append(ok, n)
		}
	}
	slices.Sort(ok)
	var parts []string
	for i := 0; i < len(ok); {
		j := i
		for j+1 < len(ok) && ok[j+1] == ok[j]+1 {
			j++
		}
		s := w.text[w.pieces[ok[i]-1].start:w.pieces[ok[j]-1].end]
		if s = strings.TrimSpace(strings.TrimRight(s, ",;")); s != "" {
			parts = append(parts, s)
		}
		i = j + 1
	}
	return parts
}

// runs is spans joined with " … ", or "" when no number is valid.
func (w *wording) runs(nums []int) string { return strings.Join(w.spans(nums), " … ") }

// said is what a draft keeps as the owner's wording: the chosen pieces, or the whole text
// when none is valid, so the words are never lost. A re-optimized rule keeps its first
// wording and gets the new words it chose, if any, on a line under it.
func (w *wording) said(nums []int) string {
	s := w.runs(nums)
	if prior := strings.TrimSpace(w.prior); prior != "" {
		if s == "" {
			return prior
		}
		return prior + "\n" + s
	}
	if s == "" {
		s = strings.TrimSpace(w.text)
	}
	return s
}
