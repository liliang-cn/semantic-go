package semantic

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A client has several published domains, not one model. Routing between them
// happens before any other question, so the catalog has to be stable, has to
// refuse ambiguity, and has to carry the one line routing is done on.
func TestCatalogFromDirectory(t *testing.T) {
	c, err := LoadCatalog("testdata")
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	if c.Len() < 2 {
		t.Fatalf("want several domains, got %v", c.Names())
	}
	// A model that names itself beats the file it arrived in: files get
	// renamed and copied, and a domain an agent routed to yesterday should
	// still be there today.
	if !containsName(c.Names(), "meridian_retail") {
		t.Errorf("a model's own name should win over its filename: %v", c.Names())
	}
	for _, d := range c.Domains() {
		if d.Description == "" {
			t.Errorf("domain %q has no description — routing has nothing to go on", d.Name)
		}
		if d.Metrics == 0 || d.Datasets == 0 {
			t.Errorf("domain %q reports %d metrics / %d datasets", d.Name, d.Metrics, d.Datasets)
		}
	}
}

// Naming is required once there is more than one domain. Guessing would answer
// a billing question out of the rostering model: a real number about the wrong
// thing, and nothing in the answer would say so.
func TestCatalogRefusesAmbiguity(t *testing.T) {
	c, err := LoadCatalog("testdata")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(""); err == nil {
		t.Error("an unnamed domain must be refused when several are published")
	} else if !strings.Contains(err.Error(), "name one") {
		t.Errorf("the refusal should list what is published, got: %v", err)
	}
	if _, err := c.Get("nope"); err == nil || !strings.Contains(err.Error(), "published:") {
		t.Errorf("an unknown domain should name the alternatives, got: %v", err)
	}

	// One domain: the name is implicit, and the caller is not made to route.
	one, err := LoadCatalog("testdata/shifts.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := one.Get(""); err != nil {
		t.Errorf("a single domain should not need naming: %v", err)
	}
}

// Two domains under one name means one of them can never be routed to, and
// which one is an accident of load order.
func TestCatalogRefusesDuplicateNames(t *testing.T) {
	m, err := LoadFile("testdata/shifts.yaml")
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewCatalog(Domain{Name: "dup", Model: m}, Domain{Name: "dup", Model: m})
	if err == nil {
		t.Fatal("expected a refusal for two domains sharing a name")
	}
	if !strings.Contains(err.Error(), "accident of load order") {
		t.Errorf("unhelpful refusal: %v", err)
	}
}

// The routing list has to be stable between runs: an agent keeping it in
// context should not see it reshuffle because a filesystem felt like it.
func TestCatalogOrderIsStable(t *testing.T) {
	first, err := LoadCatalog("testdata")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		again, err := LoadCatalog("testdata")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(again.Names(), ",") != strings.Join(first.Names(), ",") {
			t.Fatalf("order changed between loads: %v then %v", first.Names(), again.Names())
		}
	}
}

// A directory mixing native models and interchange documents loads as one
// catalog: which format a domain arrived in is not something an agent should
// be able to tell.
func TestCatalogMixesFormats(t *testing.T) {
	dir := t.TempDir()
	for _, src := range []string{"testdata/shifts.yaml", "testdata/ossie/shifts_ossie.yaml"} {
		data, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, filepath.Base(src)), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Both fixtures describe the same domain, so loading them together is the
	// duplicate case — which is itself the right answer.
	if _, err := LoadCatalog(dir); err == nil || !strings.Contains(err.Error(), "both named") {
		t.Fatalf("want a duplicate-name refusal, got: %v", err)
	}
}
