package web

import (
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// Review finding 15: a double-submitted form must post once. The busy()
// guard is lifted out of app.js and exercised under node with a stub form.
func TestAppBusyGuardsDoubleSubmit(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	src, err := os.ReadFile("assets/app.js")
	if err != nil {
		t.Fatal(err)
	}
	fn := regexp.MustCompile(`(?s)async function busy\(form, fn\) \{.*?\n\}\n`).Find(src)
	if fn == nil {
		t.Fatal("busy() not found in app.js")
	}
	script := string(fn) + `
const btn = { disabled: false };
const form = { dataset: {}, querySelectorAll: () => [btn] };
let posts = 0, sawDisabled = false;
const post = async () => { posts++; sawDisabled = btn.disabled; await new Promise((r) => setTimeout(r, 20)); };
Promise.all([busy(form, post), busy(form, post)]).then(async () => {
  await busy(form, post); // usable again afterwards
  if (posts !== 2 || !sawDisabled || btn.disabled) { console.log("FAIL", posts, sawDisabled, btn.disabled); process.exit(1); }
  console.log("ok");
});
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "ok") {
		t.Fatalf("busy guard: %v %s", err, out)
	}
}
