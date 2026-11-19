package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"syscall"
)

type feedReleaseOutcome struct {
	Released bool
	Detail   string
}

func feedReleaseRefusal(detail string) *feedWeightRefusal {
	return feedSharedRefusal(feedWeightCauseSharedOwner, detail)
}

func newFeedWeightReleaser(logf func(string, ...any)) *feedWeightAuthority {
	a := newFeedWeightAuthority("release", "", logf)
	a.key, a.role, a.configPath = "", "release", ""
	return a
}

func (a *feedWeightAuthority) releaseOwner(ctx context.Context, key string) (feedReleaseOutcome, error) {
	var out feedReleaseOutcome
	if !feedValidOwnerKey(key) {
		return out, fmt.Errorf("owner key %q is not a %d-character lowercase hexadecimal shared weight owner key (see the owner list in the shared weight status)", key, 2*feedAuthorityKeyBytes)
	}
	if fi, err := os.Lstat(a.dir); errors.Is(err, os.ErrNotExist) {
		out.Detail = fmt.Sprintf("the shared weight directory %s does not exist; owner %s holds no reserve", a.dir, key)
		return out, nil
	} else if err != nil {
		return out, fmt.Errorf("shared weight directory %s: %v", a.dir, err)
	} else if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return out, fmt.Errorf("%s is not a real directory", a.dir)
	}
	err := a.tx(ctx, "release", func(t *feedAuthTx) error {
		row := t.rec.Owners[key]
		ev, evExists, err := feedReadOnePolicyEvidence(a.dir, key)
		if err != nil {
			return feedSharedRefusal(feedWeightCauseSharedStorage, fmt.Sprintf("read owner policy evidence %s: %v", feedAuthorityPolicyPath(a.dir, key), err))
		}
		heldInRecord := row != nil && (row.Policy.Reserve > 0 || row.Unproven || row.Active)
		heldInEvidence := evExists && (ev == nil || ev.Staged != nil || ev.held().Reserve > 0)
		lockPath := feedAuthorityOwnerLockPath(a.dir, key)
		f, oerr := os.OpenFile(lockPath, os.O_RDWR, 0)
		if errors.Is(oerr, os.ErrNotExist) {
			if !heldInRecord && !heldInEvidence {
				out.Detail = fmt.Sprintf("owner %s holds no reserve; nothing to release", key)
				return nil
			}
			return feedReleaseRefusal(fmt.Sprintf("owner %s holds a reserve but its lifetime lock %s is missing, so its liveness cannot be proved; release refused", key, lockPath))
		}
		if oerr != nil {
			return feedReleaseRefusal(fmt.Sprintf("lifetime lock %s cannot be opened (%v), so the liveness of owner %s cannot be proved; release refused", lockPath, oerr, key))
		}
		held := false
		t.onFinally = append(t.onFinally, func() {
			if held {
				syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			}
			f.Close()
		})
		if err := feedAuthorityHookAt("release:lock"); err != nil {
			return feedReleaseRefusal(fmt.Sprintf("lifetime lock %s: %v", lockPath, err))
		}
		if ferr := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); ferr != nil {
			if errors.Is(ferr, syscall.EWOULDBLOCK) {
				return feedReleaseRefusal(fmt.Sprintf("owner %s is live: another process holds its lifetime lock %s; stop it first, release refused", key, lockPath))
			}
			return feedReleaseRefusal(fmt.Sprintf("lifetime lock %s cannot be locked (%v), so the liveness of owner %s cannot be proved; release refused", lockPath, ferr, key))
		}
		held = true
		heldFi, herr := f.Stat()
		curFi, cerr := os.Lstat(lockPath)
		if herr != nil || cerr != nil {
			return feedReleaseRefusal(fmt.Sprintf("lifetime lock %s cannot be identified (%v %v); release refused", lockPath, herr, cerr))
		}
		hid, ok1 := feedFileIDOf(heldFi)
		cid, ok2 := feedFileIDOf(curFi)
		if !ok1 || !ok2 || hid != cid {
			return feedReleaseRefusal(fmt.Sprintf("lifetime lock %s was replaced while it was checked; lock files are never replaced, release refused", lockPath))
		}
		if !heldInRecord && !heldInEvidence {
			out.Detail = fmt.Sprintf("owner %s holds no reserve; already released", key)
			return nil
		}
		if row == nil {
			if ev != nil {
				row = feedHeldOwnerRow(ev.feedAuthIdentity, ev.held(), ev.Tx, t.now.Ns)
			} else {
				row = feedUnprovenOwnerRow(feedAuthIdentity{}, t.now.Ns)
			}
			t.rec.Owners[key] = row
		}
		if row.Active {
			t.ownerDied(key, row, "release proved its lifetime lock free")
		}
		id, reserve := row.identity(), row.Policy.Reserve
		if err := feedAuthorityHookAt("release:before_stage"); err != nil {
			return feedSharedRefusal(feedWeightCauseSharedStorage, fmt.Sprintf("release owner %s: %v", key, err))
		}
		tx, sr := t.stagePolicy(key, id, feedAuthPolicy{}, true)
		if sr != nil {
			return sr
		}
		row.Policy, row.PolicyTx, row.Unproven = feedAuthPolicy{}, tx, false
		charges, debt, bounds := 0, 0, len(row.Bounds)
		for _, c := range row.Charges {
			charges += c.Weight
			if c.Debt {
				debt += c.Weight
			}
		}
		t.dirty = true
		line := fmt.Sprintf("[feed-weight-shared] released owner=%s role=%s config=%s reserve=%d->0 charges_kept=%d debt_kept=%d bounds_kept=%d", key, id.Role, id.ConfigPath, reserve, charges, debt, bounds)
		t.onCommit = append(t.onCommit, func() {
			out.Released = true
			out.Detail = line
		})
		return nil
	})
	return out, err
}

func runFeedWeightRelease(args []string) int {
	fs := flag.NewFlagSet("feed-weight-release", flag.ContinueOnError)
	owner := fs.String("owner", "", "Shared weight owner key whose held reserve is released (the stopped owner must not be running)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 || *owner == "" {
		fmt.Fprintln(os.Stderr, "usage: go-trader feed-weight-release --owner <key>")
		return 2
	}
	a := newFeedWeightReleaser(func(format string, v ...any) { fmt.Fprintf(os.Stderr, format+"\n", v...) })
	ctx, cancel := context.WithTimeout(context.Background(), 2*feedAuthorityLockWait+feedAuthorityStopBound)
	defer cancel()
	out, err := a.releaseOwner(ctx, *owner)
	if err != nil {
		fmt.Fprintf(os.Stderr, "feed-weight-release: refused: %v\n", err)
		return 1
	}
	fmt.Println(out.Detail)
	return 0
}
