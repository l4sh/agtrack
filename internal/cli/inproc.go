package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/l4sh/agtrack/internal/cli/argp"
	"github.com/l4sh/agtrack/internal/db"
	"github.com/l4sh/agtrack/internal/errs"
	"github.com/l4sh/agtrack/internal/store"
)

// inproc runs agtrack verbs in this process on a shared connection, with
// the CLI's own parser, validation, events, idempotency receipts and
// attribution. The browse backend and `batch` both use it.
type inproc struct {
	db      *db.DB
	actor   string // writes are attributed to this actor
	session string // stamped on writes (empty: none)
	context string // forces the context when non-empty
	cwdRepo func() string
	// checkActor applies the actor registry rules (actor.required/strict).
	checkActor bool
	// pre are global flags prepended to every argv.
	pre []string
	// vet inspects a parsed op and may refuse it.
	vet func(res *argp.Result) error
}

// run executes argv and returns its -f json output, decoded. expectTitle is
// the browse app's title compare-and-set precondition for `task set` /
// `sub set`, checked by the store inside the write transaction.
func (r *inproc) run(expectTitle *string, argv ...string) (any, error) {
	full := append(append([]string{}, r.pre...), argv...)
	res, err := NewParser().Parse(full)
	if err != nil {
		if _, ok := errs.As(err); ok {
			return nil, err
		}
		return nil, errs.Usage("%s", err.Error())
	}
	if r.vet != nil {
		if err := r.vet(res); err != nil {
			return nil, err
		}
	}
	var out, errb bytes.Buffer
	c, err := newCtx(res, Streams{Out: &out, Err: &errb})
	if err != nil {
		return nil, err
	}
	h, ok := res.Cmd.Run.(Handler)
	if !ok {
		return nil, errs.Usage("not a command: %s", strings.Join(res.Path, " "))
	}
	if r.actor != "" {
		c.Actor = r.actor
	}
	if r.context != "" {
		c.Context = r.context
	}
	c.Session = r.session
	s := store.New(r.db, c.Actor)
	s.Context = c.Context
	s.Session = r.session
	s.CwdRepo = r.cwdRepo
	c.S = s
	if r.checkActor {
		if err := c.checkActor(res); err != nil {
			return nil, err
		}
	}
	c.expectTitle = expectTitle
	if c.idemKey != "" {
		if !idemVerbs[strings.Join(res.Path, " ")] {
			return nil, errs.Usage("--idem is only accepted by create verbs")
		}
		c.idemSHA = requestSHA(res.Path, res.Values, c.Actor, c.Context)
		rc, hit, err := c.S.CheckReceipt(c.idemKey, c.idemSHA)
		if err != nil {
			return nil, err
		}
		if hit {
			c.receipt = rc
		}
	}
	if err := h(c); err != nil {
		return nil, err
	}
	if out.Len() == 0 {
		return nil, nil
	}
	dec := json.NewDecoder(&out)
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("%s: unexpected output: %v", strings.Join(res.Path, " "), err)
	}
	return plainJSON(v), nil
}
