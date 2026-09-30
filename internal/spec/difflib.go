package spec

import (
	"fmt"
	"sort"
	"strings"
)

// matcher is a port of Python's difflib.SequenceMatcher with isjunk=None and
// autojunk=True: in a b of 200+ elements, any element occurring more than
// len(b)/100+1 times is "popular" and never seeds a match (it can still
// extend one). Ports of find_longest_match, get_matching_blocks and
// get_opcodes follow CPython 3.12 line for line so results agree exactly.
type matcher[T comparable] struct {
	a, b []T
	b2j  map[T][]int
}

func newMatcher[T comparable](a, b []T) *matcher[T] {
	m := &matcher[T]{a: a, b: b, b2j: map[T][]int{}}
	for j, e := range b {
		m.b2j[e] = append(m.b2j[e], j)
	}
	if n := len(b); n >= 200 {
		ntest := n/100 + 1
		for e, idx := range m.b2j {
			if len(idx) > ntest {
				delete(m.b2j, e)
			}
		}
	}
	return m
}

type block struct{ i, j, k int }

func (m *matcher[T]) longest(alo, ahi, blo, bhi int) block {
	a, b := m.a, m.b
	besti, bestj, bestsize := alo, blo, 0
	j2len := map[int]int{}
	for i := alo; i < ahi; i++ {
		newj2len := map[int]int{}
		for _, j := range m.b2j[a[i]] {
			if j < blo {
				continue
			}
			if j >= bhi {
				break
			}
			k := j2len[j-1] + 1
			newj2len[j] = k
			if k > bestsize {
				besti, bestj, bestsize = i-k+1, j-k+1, k
			}
		}
		j2len = newj2len
	}
	// With no isjunk every element is non-junk, so only these two loops
	// apply; they extend the match across popular elements.
	for besti > alo && bestj > blo && a[besti-1] == b[bestj-1] {
		besti, bestj, bestsize = besti-1, bestj-1, bestsize+1
	}
	for besti+bestsize < ahi && bestj+bestsize < bhi && a[besti+bestsize] == b[bestj+bestsize] {
		bestsize++
	}
	return block{besti, bestj, bestsize}
}

func (m *matcher[T]) matchingBlocks() []block {
	la, lb := len(m.a), len(m.b)
	queue := [][4]int{{0, la, 0, lb}}
	var blocks []block
	for len(queue) > 0 {
		q := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		alo, ahi, blo, bhi := q[0], q[1], q[2], q[3]
		x := m.longest(alo, ahi, blo, bhi)
		if x.k > 0 {
			blocks = append(blocks, x)
			if alo < x.i && blo < x.j {
				queue = append(queue, [4]int{alo, x.i, blo, x.j})
			}
			if x.i+x.k < ahi && x.j+x.k < bhi {
				queue = append(queue, [4]int{x.i + x.k, ahi, x.j + x.k, bhi})
			}
		}
	}
	sort.Slice(blocks, func(p, q int) bool {
		if blocks[p].i != blocks[q].i {
			return blocks[p].i < blocks[q].i
		}
		if blocks[p].j != blocks[q].j {
			return blocks[p].j < blocks[q].j
		}
		return blocks[p].k < blocks[q].k
	})
	var out []block
	i1, j1, k1 := 0, 0, 0
	for _, x := range blocks {
		if i1+k1 == x.i && j1+k1 == x.j {
			k1 += x.k
		} else {
			if k1 > 0 {
				out = append(out, block{i1, j1, k1})
			}
			i1, j1, k1 = x.i, x.j, x.k
		}
	}
	if k1 > 0 {
		out = append(out, block{i1, j1, k1})
	}
	return append(out, block{la, lb, 0})
}

func (m *matcher[T]) ratio() float64 {
	t := len(m.a) + len(m.b)
	if t == 0 {
		return 1.0
	}
	matches := 0
	for _, x := range m.matchingBlocks() {
		matches += x.k
	}
	return 2.0 * float64(matches) / float64(t)
}

type opcode struct {
	tag            byte // 'r'eplace, 'd'elete, 'i'nsert, 'e'qual
	i1, i2, j1, j2 int
}

func (m *matcher[T]) opcodes() []opcode {
	i, j := 0, 0
	var out []opcode
	for _, x := range m.matchingBlocks() {
		var tag byte
		switch {
		case i < x.i && j < x.j:
			tag = 'r'
		case i < x.i:
			tag = 'd'
		case j < x.j:
			tag = 'i'
		}
		if tag != 0 {
			out = append(out, opcode{tag, i, x.i, j, x.j})
		}
		i, j = x.i+x.k, x.j+x.k
		if x.k > 0 {
			out = append(out, opcode{'e', x.i, i, x.j, j})
		}
	}
	return out
}

func (m *matcher[T]) groupedOpcodes(n int) [][]opcode {
	codes := m.opcodes()
	if len(codes) == 0 {
		codes = []opcode{{'e', 0, 1, 0, 1}}
	}
	if c := &codes[0]; c.tag == 'e' {
		c.i1, c.j1 = max(c.i1, c.i2-n), max(c.j1, c.j2-n)
	}
	if c := &codes[len(codes)-1]; c.tag == 'e' {
		c.i2, c.j2 = min(c.i2, c.i1+n), min(c.j2, c.j1+n)
	}
	nn := n + n
	var groups [][]opcode
	var group []opcode
	for _, c := range codes {
		if c.tag == 'e' && c.i2-c.i1 > nn {
			group = append(group, opcode{'e', c.i1, min(c.i2, c.i1+n), c.j1, min(c.j2, c.j1+n)})
			groups = append(groups, group)
			group = nil
			c.i1, c.j1 = max(c.i1, c.i2-n), max(c.j1, c.j2-n)
		}
		group = append(group, c)
	}
	if len(group) > 0 && !(len(group) == 1 && group[0].tag == 'e') {
		groups = append(groups, group)
	}
	return groups
}

// NormalizeBody is the form bodies are compared in for similarity: trailing
// whitespace dropped from every line, runs of blank lines collapsed to one,
// blank edges removed.
func NormalizeBody(body string) string {
	var out []string
	blank := false
	for _, ln := range strings.Split(body, "\n") {
		ln = strings.TrimRight(ln, " \t")
		if ln == "" {
			blank = len(out) > 0
			continue
		}
		if blank {
			out = append(out, "")
		}
		blank = false
		out = append(out, ln)
	}
	return strings.Join(out, "\n")
}

// Similarity is difflib.SequenceMatcher(None, a, b).ratio() over the
// characters (code points, as Python sees them) of a and b. Callers
// normalise with NormalizeBody first.
func Similarity(a, b string) float64 {
	return newMatcher([]rune(a), []rune(b)).ratio()
}

// UnifiedDiff is difflib.unified_diff(a.splitlines(), b.splitlines(),
// labelA, labelB, n=context, lineterm=""), each output line terminated by
// "\n". Identical inputs give "".
func UnifiedDiff(a, b, labelA, labelB string, context int) string {
	al, bl := splitLines(a), splitLines(b)
	var sb strings.Builder
	for gi, group := range newMatcher(al, bl).groupedOpcodes(context) {
		if gi == 0 {
			fmt.Fprintf(&sb, "--- %s\n+++ %s\n", labelA, labelB)
		}
		first, last := group[0], group[len(group)-1]
		fmt.Fprintf(&sb, "@@ -%s +%s @@\n", unifiedRange(first.i1, last.i2), unifiedRange(first.j1, last.j2))
		for _, c := range group {
			if c.tag == 'e' {
				for _, ln := range al[c.i1:c.i2] {
					sb.WriteString(" " + ln + "\n")
				}
				continue
			}
			if c.tag == 'r' || c.tag == 'd' {
				for _, ln := range al[c.i1:c.i2] {
					sb.WriteString("-" + ln + "\n")
				}
			}
			if c.tag == 'r' || c.tag == 'i' {
				for _, ln := range bl[c.j1:c.j2] {
					sb.WriteString("+" + ln + "\n")
				}
			}
		}
	}
	return sb.String()
}

func unifiedRange(start, stop int) string {
	beginning, length := start+1, stop-start
	if length == 1 {
		return fmt.Sprint(beginning)
	}
	if length == 0 {
		beginning--
	}
	return fmt.Sprintf("%d,%d", beginning, length)
}

// splitLines is str.splitlines() for LF-normalised text.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}
