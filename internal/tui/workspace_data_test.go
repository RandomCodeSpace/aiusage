package tui

import (
	"reflect"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage/model"
	"github.com/RandomCodeSpace/aiusage/store"
)

func TestWorkspaceProviderCrumbsPreserveScope(t *testing.T) {
	d := NewData(&fakeData{})
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	sp := Span{R: Range7d, Step: -1}
	since, until := sp.Window(now)
	for _, tc := range []struct {
		name      string
		providers []string
	}{
		{"all", nil},
		{"named", []string{"openai"}},
		{"unknown", []string{""}},
		{"multiple", []string{"openai", ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			crumbs := []Crumb{
				{Dim: "model", Value: "model-a"},
				{Dim: "tool", Value: "codex"},
				{Dim: "project", Value: "/project"},
				{Dim: "session", Value: "session-a"},
			}
			for _, provider := range tc.providers {
				crumbs = append(crumbs, Crumb{Dim: "provider", Value: provider})
			}
			usage := d.filterFor(t.Context(), now, sp, crumbs, []string{"day"})
			wantUsage := store.Filter{Since: since, Until: until, Tools: []string{"codex"}, Models: []string{"model-a"}, Projects: []string{"/project"}, Sessions: []string{"session-a"}, Providers: tc.providers, GroupBy: []string{"day"}}
			if !reflect.DeepEqual(usage, wantUsage) {
				t.Fatalf("usage scope = %+v, want %+v", usage, wantUsage)
			}
			wantUsage.GroupBy = nil
			window := windowTotalsFilter(since, until, crumbs)
			if !reflect.DeepEqual(window, wantUsage) {
				t.Fatalf("explicit window scope = %+v, want %+v", window, wantUsage)
			}
			wantActivity := store.ActivityFilter{Since: since, Until: until, Tools: []string{"codex"}, Models: []string{"model-a"}, Projects: []string{"/project"}, Sessions: []string{"session-a"}, Providers: tc.providers, GroupBy: []string{"name"}}
			activity := d.activityFilterFor(now, sp, crumbs, []string{"name"})
			if !reflect.DeepEqual(activity, wantActivity) {
				t.Fatalf("activity scope = %+v, want %+v", activity, wantActivity)
			}
			wantActivity.GroupBy = []string{"value"}
			turns := d.turnContextFilterFor(now, sp, crumbs, []string{"value"})
			if !reflect.DeepEqual(turns, wantActivity) {
				t.Fatalf("turn scope = %+v, want %+v", turns, wantActivity)
			}
		})
	}
}

func TestWorkspaceProviderCachesSeparateUnknownFromAll(t *testing.T) {
	// An unknown-provider breadcrumb must never retrieve the unfiltered answer
	// after totals or activity have already warmed the cache.
	scopes := []struct {
		name      string
		providers []string
	}{
		{"all", nil},
		{"unknown", []string{""}},
		{"openai", []string{"openai"}},
		{"anthropic", []string{"anthropic"}},
		{"openai and unknown", []string{"openai", ""}},
	}
	for name, key := range map[string]func([]string) string{
		"usage": func(p []string) string { return cacheKey(store.Filter{Models: []string{"model-a"}, Providers: p}) },
		"activity": func(p []string) string {
			return activityKey(store.ActivityFilter{Models: []string{"model-a"}, Providers: p})
		},
		"turn context": func(p []string) string {
			return turnContextKey(model.DimensionSkill, store.ActivityFilter{Models: []string{"model-a"}, Providers: p})
		},
	} {
		t.Run(name, func(t *testing.T) {
			seen := map[string]string{}
			for _, scope := range scopes {
				got := key(scope.providers)
				if previous, ok := seen[got]; ok {
					t.Errorf("%s and %s share cache key %q", previous, scope.name, got)
				}
				seen[got] = scope.name
			}
			if key(nil) != key([]string{}) {
				t.Error("nil and empty providers select the same population but use different cache keys")
			}
		})
	}
}
