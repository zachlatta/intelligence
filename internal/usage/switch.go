package usage

import (
	"context"
	"fmt"
	"strconv"
)

// SwitchClaude makes login the Claude login cswap uses on this host, for every Claude agent on it,
// and returns its slot. The slot is read from `cswap list --json` now, since slots can change.
// It switches without --force, so cswap first backs up the outgoing login's live credentials
// (a refreshed token is kept), and it checks that cswap then lists login as active.
func (r *Reader) SwitchClaude(ctx context.Context, login string) (int, error) {
	find := func() (Account, error) {
		out, err := r.run(ctx, "cswap", "list", "--json")
		if err != nil {
			return Account{}, fmt.Errorf("cswap list: %w", err)
		}
		s := parseClaude(out, r.now())
		if s.Error != "" {
			return Account{}, fmt.Errorf("%s", s.Error)
		}
		for _, a := range s.Accounts {
			if a.ID == login {
				return a, nil
			}
		}
		return Account{}, fmt.Errorf("cswap holds no login %s on this host", login)
	}
	a, err := find()
	if err != nil {
		return 0, err
	}
	switch {
	case a.Active:
		return a.Slot, nil
	case a.Slot <= 0:
		return 0, fmt.Errorf("cswap lists %s with no slot", login)
	case a.Error != "":
		return 0, fmt.Errorf("not switching to %s: %s", login, a.Error)
	}
	if _, err := r.run(ctx, "cswap", "switch", strconv.Itoa(a.Slot), "--json"); err != nil {
		return a.Slot, fmt.Errorf("cswap switch %d: %w", a.Slot, err)
	}
	after, err := find()
	if err != nil {
		return a.Slot, err
	}
	if !after.Active {
		return a.Slot, fmt.Errorf("cswap switch %d ran, but %s is still not the active login", a.Slot, login)
	}
	return after.Slot, nil
}
