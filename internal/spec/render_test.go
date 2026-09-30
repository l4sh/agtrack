package spec

import (
	"strings"
	"testing"
)

func TestLocateQuote(t *testing.T) {
	body := "Refresh tokens rotate on every use. The previous token stays valid for a grace\nwindow of 60s to absorb in-flight requests. Every use counts."
	cases := []struct {
		quote, prefix, suffix string
		state                 QuoteState
		off                   int
	}{
		{"grace window of 60s", "", "", QuoteOK, strings.Index(body, "grace\nwindow")},
		{"  valid   for a ", "", "", QuoteOK, strings.Index(body, "valid for")},
		{"use", "", "", QuoteAmbiguous, -1},
		{"use", "on every", "", QuoteOK, strings.Index(body, "use.")},
		{"use", "Every", "counts", QuoteOK, strings.LastIndex(body, "use")},
		{"use", "nowhere", "", QuoteAmbiguous, -1},
		{"90s", "", "", QuoteMissing, -1},
		{"   ", "", "", QuoteMissing, -1},
	}
	for _, c := range cases {
		st, off := LocateQuote(body, c.quote, c.prefix, c.suffix)
		if st != c.state || off != c.off {
			t.Errorf("LocateQuote(%q, %q, %q) = %s %d, want %s %d", c.quote, c.prefix, c.suffix, st, off, c.state, c.off)
		}
	}
	// Offsets are bytes, correct past multibyte text.
	st, off := LocateQuote("Überblick — schnell", "schnell", "", "")
	if st != QuoteOK || off != strings.Index("Überblick — schnell", "schnell") {
		t.Errorf("multibyte %s %d", st, off)
	}
}

func TestEscapeFTS(t *testing.T) {
	for in, want := range map[string]string{
		"token rotation":       `"token" "rotation"`,
		`say "hi" NOT OR*`:     `"say" """hi""" "NOT" "OR*"`,
		"  heading:body  x^2 ": `"heading:body" "x^2"`,
		"":                     ``,
		"grace\twindow\n(60s)": `"grace" "window" "(60s)"`,
	} {
		if got := EscapeFTS(in); got != want {
			t.Errorf("EscapeFTS(%q) = %s want %s", in, got, want)
		}
	}
}

func TestRenderHTML(t *testing.T) {
	body := strings.Join([]string{
		"Text with <b>inline html</b> and <script>alert(1)</script>.",
		"",
		"<div onclick=\"x()\">block html</div>",
		"",
		"[js](javascript:alert(1)) [tab](java\tscript:alert(1)) [ent](&#106;avascript:alert(1)) [data](data:text/html;base64,AA) [dimg](data:image/png;base64,AA) [ok](https://example.com/a?b=1&c=2)",
		"",
		"<javascript:alert(1)> <https://example.com>",
		"",
		"![ok](data:image/png;base64,AA) ![bad](data:image/svg+xml;base64,AA) ![js](javascript:x) ![rel](img/a.png)",
		"",
		"| a | b |",
		"|---|---|",
		"| 1 | 2 |",
		"",
		"```mermaid",
		"graph LR; A-->B<script>",
		"```",
		"",
		"```go",
		"x := \"<y>\"",
		"```",
	}, "\n")
	out, err := RenderHTML(body)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"<script", "<b>", "<div", "onclick", `="javascript`, "data:text", "data:image/svg", "raw HTML omitted", `href="data:`} {
		if strings.Contains(strings.ToLower(out), strings.ToLower(bad)) {
			t.Errorf("output contains %q:\n%s", bad, out)
		}
	}
	for _, good := range []string{
		`<pre class="mermaid-src" data-mermaid="1"><code>graph LR; A--&gt;B&lt;script&gt;` + "\n</code></pre>",
		`<pre><code class="language-go">x := &quot;&lt;y&gt;&quot;`,
		"<table>", "<td>1</td>",
		`<img src="data:image/png;base64,AA" alt="ok">`,
		`<img src="img/a.png" alt="rel">`,
		`<a href="https://example.com/a?b=1&amp;c=2">ok</a>`,
		`<a href="https://example.com">https://example.com</a>`,
	} {
		if !strings.Contains(out, good) {
			t.Errorf("output lacks %q:\n%s", good, out)
		}
	}
}

func TestLocateQuoteRenderedSelection(t *testing.T) {
	body := "Refresh a **grace window** of 60s. See [RFC 6749](https://x) for `refresh_token` &amp; more.\n\n- item one\n- item two\n\nEarlier the cap *is* 5. Later the cap *is* 9."
	cases := []struct {
		quote, prefix, suffix string
		state                 QuoteState
		at                    string // source text the offset must point at
	}{
		{"a grace window of", "", "", QuoteOK, "a **grace"},
		{"See RFC 6749 for", "", "", QuoteOK, "See [RFC"},
		{"for refresh_token & more", "", "", QuoteOK, "for `refresh_token`"},
		{"item one item two", "", "", QuoteOK, "item one"},
		{"the cap is", "Later ", "", QuoteOK, "the cap *is* 9"},
		{"the cap is", "", "", QuoteAmbiguous, ""},
		{"not there", "", "", QuoteMissing, ""},
	}
	for _, c := range cases {
		st, off := LocateQuote(body, c.quote, c.prefix, c.suffix)
		if st != c.state || (c.at != "" && (off < 0 || !strings.HasPrefix(body[off:], c.at))) {
			t.Errorf("LocateQuote(%q) = %s %d, want %s at %q", c.quote, st, off, c.state, c.at)
		}
	}
	pt, offs := PlainText(body)
	if len(offs) != len(pt)+1 || strings.Contains(pt, "**") || strings.Contains(pt, "https://x") || !strings.Contains(pt, "refresh_token & more") {
		t.Errorf("PlainText %q", pt)
	}
}
