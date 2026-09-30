package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"

	"github.com/l4sh/agtrack/internal/store"
)

// Idempotency (design §4.4). `--idem KEY` on a create verb. The dispatcher
// checks the receipt before the handler runs — before any other
// precondition — so a retry after a successful create reprints the original
// result even if the world has moved on. The receipt is written in the same
// transaction as the create's effects.

// renderFlags never count towards a request's identity.
var renderFlags = map[string]bool{
	"db": true, "format": true, "fields": true, "extended": true, "no_header": true,
	"color": true, "no_color": true, "quiet": true, "idem": true, "max_bytes": true,
	"count": true, "ids": true,
}

// requestSHA canonicalises the operation, its explicit inputs and the actor.
func requestSHA(path []string, values map[string]any, actor, context string) string {
	keys := make([]string, 0, len(values))
	for k := range values {
		if !renderFlags[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	canon := map[string]any{"op": strings.Join(path, " "), "actor": actor, "context": context}
	in := map[string]any{}
	for _, k := range keys {
		if values[k] != nil && values[k] != false {
			in[k] = values[k]
		}
	}
	canon["in"] = in
	b, _ := json.Marshal(canon)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// replay reprints a create recorded under the same key. done=true means the
// handler must return err without doing anything else.
func (c *Ctx) replay(show func(ids []int64) error) (done bool, err error) {
	if c.receipt == nil {
		return false, nil
	}
	return true, show(c.receipt.TargetIDs)
}

// record runs create, writing the receipt in the same transaction when a
// key was given.
func (c *Ctx) record(kind string, create func() ([]int64, error)) ([]int64, error) {
	if c.idemKey == "" {
		return create()
	}
	var ids []int64
	err := c.S.Tx(func() error {
		var err error
		if ids, err = create(); err != nil {
			return err
		}
		return c.S.WriteReceipt(c.idemKey, c.idemSHA, kind, ids)
	})
	return ids, err
}

// idem holds the per-invocation idempotency state.
type idem struct {
	idemKey string
	idemSHA string
	receipt *store.Receipt
}
