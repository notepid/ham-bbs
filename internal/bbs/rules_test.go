package bbs

import (
	"slices"
	"testing"

	"github.com/notepid/ham-bbs/internal/config"
)

// TestRouting checks the hub example in docs/setup.md ("Routes and
// scopes by example").
func TestRouting(t *testing.T) {
	s := &Server{cfg: config.Server{Node: "LA2HUB"}}
	for _, n := range []config.Neighbor{
		{Node: "LA1OSL", Scopes: []string{"*"}},
		{Node: "LA9BRG", Routes: []string{"LA9*"}, Scopes: []string{"ALL", "LA"}},
		{Node: "SM5BBS", Routes: []string{"*"}, Scopes: []string{"ALL", "EU"}},
	} {
		s.neighbors = append(s.neighbors, &neighbor{Neighbor: n})
	}

	for at, want := range map[string]string{
		"LA1OSL":            "LA1OSL",
		"LA1OSL.OSL.NOR.EU": "LA1OSL",
		"LA9XYZ":            "LA9BRG",
		"LA9XYZ.NOR.EU":     "LA9BRG",
		"SK0BBS":            "SM5BBS",
		"W1AW":              "SM5BBS",
	} {
		if nb := s.routeFor(at); nb == nil || nb.Node != want {
			t.Errorf("mail @%s goes to %v, want %s", at, nb, want)
		}
	}
	for _, at := range []string{"", "LA2HUB", "LA2HUB.NOR.EU"} {
		if !s.isLocal(at) {
			t.Errorf("@%s is not local", at)
		}
	}

	for scope, want := range map[string][]string{
		"ALL": {"LA1OSL", "LA9BRG", "SM5BBS"},
		"LA":  {"LA1OSL", "LA9BRG"},
		"EU":  {"LA1OSL", "SM5BBS"},
	} {
		var got []string
		for _, nb := range s.neighbors {
			if nb.wantsScope(scope) {
				got = append(got, nb.Node)
			}
		}
		if !slices.Equal(got, want) {
			t.Errorf("bulletins @%s go to %v, want %v", scope, got, want)
		}
	}
}
