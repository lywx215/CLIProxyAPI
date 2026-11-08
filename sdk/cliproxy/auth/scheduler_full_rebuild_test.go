package auth

import (
	"context"
	"testing"

	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestSchedulerFullRebuildPreservesSelectionSequence(t *testing.T) {
	for _, weighted := range []bool{false, true} {
		for _, mixed := range []bool{false, true} {
			var selector Selector = &RoundRobinSelector{}
			if weighted {
				selector = &WeightedRoundRobinSelector{}
			}
			auths := []*Auth{
				{ID: "rebuild-a", Provider: "a", Attributes: map[string]string{"weight": "3"}},
				{ID: "rebuild-b", Provider: "a", Attributes: map[string]string{"weight": "2"}},
				{ID: "rebuild-c", Provider: "a", Attributes: map[string]string{"weight": "1"}},
			}
			if mixed {
				auths[2].Provider = "b"
			}
			control := newSchedulerForTest(selector, auths...)
			rebuilt := newSchedulerForTest(selector, auths...)
			pick := func(s *authScheduler) string {
				t.Helper()
				var a *Auth
				var err error
				if mixed {
					a, _, err = s.pickMixed(context.Background(), []string{"a", "b"}, "", exec.Options{}, nil)
				} else {
					a, err = s.pickSingle(context.Background(), "a", "", exec.Options{}, nil)
				}
				if err != nil {
					t.Fatal(err)
				}
				return a.ID
			}
			for i := 0; i < 30; i++ {
				if got, want := pick(rebuilt), pick(control); got != want {
					t.Fatalf("weighted=%v mixed=%v pick=%d got=%s want=%s", weighted, mixed, i, got, want)
				}
				rebuilt.rebuild(auths)
			}
		}
	}
}

func TestSchedulerFullRebuildDoesNotRestoreRemovedAuth(t *testing.T) {
	auths := []*Auth{{ID: "a", Provider: "test"}, {ID: "b", Provider: "test"}, {ID: "c", Provider: "test"}}
	s := newSchedulerForTest(&RoundRobinSelector{}, auths...)
	_, _ = s.pickSingle(context.Background(), "test", "", exec.Options{}, nil)
	s.rebuild(auths[1:])
	for _, want := range []string{"b", "c", "b"} {
		got, err := s.pickSingle(context.Background(), "test", "", exec.Options{}, nil)
		if err != nil || got.ID != want {
			t.Fatalf("got=%v err=%v want=%s", got, err, want)
		}
	}
}
