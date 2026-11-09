package paseo_test

import (
	"errors"
	"strings"
	"testing"

	. "github.com/zachlatta/intelligence/internal/paseo"
	"github.com/zachlatta/intelligence/internal/paseo/paseotest"
)

func TestCheckoutDiffReadsEverythingTheCheckoutChanged(t *testing.T) {
	f := paseotest.New(t, token)
	f.SetDiff("/w/app", paseotest.DiffFile("main.go", 2, 1), paseotest.DiffFile("logo.png", 0, 0, "binary"))
	c := dial(t, f)
	d, err := c.CheckoutDiff(ctx5(t), "/w/app")
	if err != nil {
		t.Fatal(err)
	}
	if d.TooLarge || len(d.Files) != 2 {
		t.Fatalf("diff = %+v", d)
	}
	m := d.Files[0]
	if m.Path != "main.go" || m.Additions != 2 || m.Deletions != 1 || m.Status != "ok" || len(m.Hunks) != 1 {
		t.Fatalf("file = %+v", m)
	}
	h := m.Hunks[0]
	if h.OldStart != 1 || h.NewStart != 1 || len(h.Lines) != 5 || h.Lines[0].Type != "header" || h.Lines[1].Type != "context" ||
		h.Lines[2].Type != "remove" || h.Lines[3].Type != "add" || h.Lines[3].Content != "added line 1" {
		t.Errorf("hunk = %+v", h)
	}
	if b := d.Files[1]; b.Status != "binary" || b.Hunks == nil || len(b.Hunks) != 0 {
		t.Errorf("binary file = %+v", b)
	}
	// the whole of the checkout's work: against its base, with uncommitted and untracked files
	req := f.Requests("checkout.diff.get.request")
	if len(req) != 1 || req[0]["cwd"] != "/w/app" || req[0]["compare"].(map[string]any)["mode"] != "all" {
		t.Errorf("requests = %v", req)
	}
}

func TestCheckoutDiffOfACleanCheckoutIsEmpty(t *testing.T) {
	f := paseotest.New(t, token)
	f.SetDiff("/w/app")
	d, err := dial(t, f).CheckoutDiff(ctx5(t), "/w/app")
	if err != nil || d.Files == nil || len(d.Files) != 0 {
		t.Errorf("diff = %+v, %v", d, err)
	}
}

func TestCheckoutDiffSaysWhyTheDaemonCannotReadIt(t *testing.T) {
	f := paseotest.New(t, token)
	c := dial(t, f)
	if _, err := c.CheckoutDiff(ctx5(t), "/tmp/plain"); err == nil || !strings.Contains(err.Error(), "Not a git repository: /tmp/plain") {
		t.Errorf("err = %v", err)
	}
	f.SetDiffTooLarge("/w/huge")
	if d, err := c.CheckoutDiff(ctx5(t), "/w/huge"); err != nil || !d.TooLarge || len(d.Files) != 0 {
		t.Errorf("too large: %+v, %v", d, err)
	}
}

func TestCheckoutDiffRefusalIsACheckoutError(t *testing.T) {
	f := paseotest.New(t, token)
	_, err := dial(t, f).CheckoutDiff(ctx5(t), "/tmp/plain")
	var ce *CheckoutError
	if !errors.As(err, &ce) || ce.Code != "NOT_GIT_REPO" {
		t.Errorf("err = %#v", err)
	}
}
