package uf

import (
	"strings"
	"testing"
)

func TestPlanValidateAcceptsTheFixture(t *testing.T) {
	plan := newTestLayout(t).plan(t)
	if err := plan.Validate(); err != nil {
		t.Fatalf("fixture plan does not validate: %v", err)
	}
}

func TestPlanValidateRejectsBrokenPlans(t *testing.T) {
	base := func() *Plan { return newTestLayout(t).plan(t) }

	cases := []struct {
		name   string
		mutate func(*Plan)
		want   string
	}{
		{"nil", nil, "nil plan"},
		{"nothing to write", func(p *Plan) { p.Items, p.IDB = nil, IDBPlan{} }, "nothing to write"},
		{"duplicate item", func(p *Plan) { p.Items[1].Name = p.Items[0].Name }, "appears twice"},
		{"padded size too small", func(p *Plan) { p.Items[0].PaddedSize = p.Items[0].Size - 1 }, "padded size"},
		{"unaligned padded size", func(p *Plan) { p.Items[0].PaddedSize = 5000 }, "not a multiple"},
		{"IDB without loader", func(p *Plan) { p.Loader = nil }, "no loader"},
		{"IDB not sector aligned", func(p *Plan) { p.IDB.Bytes += 1 }, "not a multiple"},
		{"item past the LBA space", func(p *Plan) { p.Items[0].TargetLBA = MaxLBA }, "past the 32-bit LBA space"},
		{"negative file offset", func(p *Plan) { p.Items[0].FileOffset = -1 }, "negative file offset"},
		{"unsectoral tail", func(p *Plan) { p.Tails[0].Bytes = 4097 }, "unsectoral"},
		{"empty item name", func(p *Plan) { p.Items[0].Name = "" }, "no name"},
	}
	for _, c := range cases {
		if c.name == "nil" {
			var p *Plan
			if err := p.Validate(); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("%s: error = %v, want %q", c.name, err, c.want)
			}
			continue
		}
		plan := base()
		c.mutate(plan)
		err := plan.Validate()
		if err == nil {
			t.Fatalf("%s: expected an error", c.name)
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: error = %v, want it to mention %q", c.name, err, c.want)
		}
	}
}

func TestPlanItemLookup(t *testing.T) {
	plan := newTestLayout(t).plan(t)
	if _, ok := plan.Item("state"); !ok {
		t.Fatal("state item not found")
	}
	if _, ok := plan.Item("missing"); ok {
		t.Fatal("missing item reported as found")
	}
}

func TestChipMatches(t *testing.T) {
	cases := []struct {
		image, device string
		want          bool
	}{
		{"RK3588", "3588", true},
		{"rk3588s", "3588", true},
		{"3588", "3588", true},
		{"RK3588", "3566", false},
		{"", "3566", true},   // no image chip: nothing to compare
		{"RK3588", "", true}, // unreadable device string: do not block
		{"RK3566/RK3568", "3588", false},
	}
	for _, c := range cases {
		if got := chipMatches(c.image, c.device); got != c.want {
			t.Fatalf("chipMatches(%q, %q) = %t, want %t", c.image, c.device, got, c.want)
		}
	}
}

func TestEraseModeParseAndDescribe(t *testing.T) {
	for _, name := range []string{"", "none", "fill", "erase-lba"} {
		if _, err := ParseEraseMode(name); err != nil {
			t.Fatalf("ParseEraseMode(%q): %v", name, err)
		}
	}
	if _, err := ParseEraseMode("erase-all"); err == nil {
		t.Fatal("expected an error for an unknown erase mode")
	}
	for _, mode := range []EraseMode{EraseNone, EraseFill, EraseLBA} {
		if !strings.Contains(mode.Describe(), "待真机核对") {
			t.Fatalf("%s does not flag the unverified erase semantics: %q", mode, mode.Describe())
		}
	}
}

func TestChipMatchesAcceptsReversedTag(t *testing.T) {
	cases := []struct {
		want, got string
		ok        bool
	}{
		{"RK3588", "8853", true},   // READ_CHIP_INFO byte-reversed tag
		{"RK3588", "RK3588", true}, // already normalised
		{"RK3588", "3588", true},
		{"RK3588", "3588S", true},
		{"RK3588", "3566", false},
		{"RK3566", "6653", true},
		{"RK3588", "8852", false},
	}
	for _, c := range cases {
		if got := chipMatches(c.want, c.got); got != c.ok {
			t.Errorf("chipMatches(%q, %q) = %t, want %t", c.want, c.got, got, c.ok)
		}
	}
}
