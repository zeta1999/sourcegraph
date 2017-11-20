package graphqlbackend

import (
	"context"
	"encoding/json"
	"errors"
	"io/ioutil"
	"sort"
	"strings"

	jsoncommentstrip "github.com/RaveNoX/go-jsoncommentstrip"
	"sourcegraph.com/sourcegraph/sourcegraph/pkg/actor"

	"sourcegraph.com/sourcegraph/sourcegraph/pkg/highlight"
)

type configurationSubject struct {
	org  *orgResolver
	user *userResolver
}

func (s *configurationSubject) ToOrg() (*orgResolver, bool) { return s.org, s.org != nil }

func (s *configurationSubject) ToUser() (*userResolver, bool) { return s.user, s.user != nil }

func (s *configurationSubject) LatestSettings(ctx context.Context) (*settingsResolver, error) {
	switch {
	case s.org != nil:
		return s.org.LatestSettings(ctx)
	case s.user != nil:
		return s.user.LatestSettings(ctx)
	}
	panic("no settings subject")
}

type configurationResolver struct {
	contents string
}

func (r *configurationResolver) Contents() string { return r.contents }

func (r *configurationResolver) Highlighted(ctx context.Context) (string, error) {
	html, aborted, err := highlight.Code(ctx, r.contents, "json", false)
	if err != nil {
		return "", err
	}
	if aborted {
		// Configuration should be small enough so the syntax highlighting
		// completes before the automatic timeout. If it doesn't, something
		// seriously wrong has happened.
		return "", errors.New("settings syntax highlighting aborted")
	}

	return string(html), nil
}

type configurationCascadeResolver struct{}

func (r *configurationCascadeResolver) Defaults() *configurationResolver {
	return &configurationResolver{
		contents: `// This is the default configuration. Override it with org or user settings.
{
  /* default configuration is empty */
}`,
	}
}

func (r *configurationCascadeResolver) Subjects(ctx context.Context) ([]*configurationSubject, error) {
	var subjects []*configurationSubject
	if actor := actor.FromContext(ctx); actor.IsAuthenticated() {
		user, err := currentUser(ctx)
		if err != nil {
			return nil, err
		}

		orgs, err := user.Orgs(ctx)
		if err != nil {
			return nil, err
		}
		// Stable-sort the orgs so that the priority of their configs is stable.
		sort.Slice(orgs, func(i, j int) bool {
			return orgs[i].org.ID < orgs[j].org.ID
		})
		// Apply the user's orgs' configuration.
		for _, org := range orgs {
			subjects = append(subjects, &configurationSubject{org: org})
		}

		// Apply the user's own configuration last (it has highest priority).
		subjects = append(subjects, &configurationSubject{user: user})
	}

	return subjects, nil
}

func (r *configurationCascadeResolver) Merged(ctx context.Context) (*configurationResolver, error) {
	configs := []string{r.Defaults().Contents()}
	subjects, err := r.Subjects(ctx)
	if err != nil {
		return nil, err
	}
	for _, s := range subjects {
		settings, err := s.LatestSettings(ctx)
		if err != nil {
			return nil, err
		}
		if settings != nil {
			configs = append(configs, settings.settings.Contents)
		}
	}

	merged, err := mergeConfigs(configs)
	if err != nil {
		return nil, err
	}
	return &configurationResolver{contents: string(merged)}, nil
}

// deeplyMergedConfigFields contains the names of top-level configuration fields whose values should
// be merged if they appear in multiple cascading configurations.
//
// For example, suppose org config is {"a":[1]} and user config is {"a":[2]}. If "a" is NOT a deeply
// merged field, the merged config would be {"a":[2]}. If "a" IS a deeply merged field, then the
// merged config would be {"a":[1,2].}
var deeplyMergedConfigFields = map[string]struct{}{}

// mergeConfigs merges the specified JSON configs together to produce a single JSON config. The merge
// algorithm is currently rudimentary but eventually it will be similar to VS Code's. The only "smart"
// merging behavior is that described in the documentation for deeplyMergedConfigFields.
//
// TODO(sqs): tolerate comments in JSON
func mergeConfigs(jsonConfigStrings []string) ([]byte, error) {
	merged := map[string]interface{}{}
	for _, s := range jsonConfigStrings {
		stripped, err := ioutil.ReadAll(jsoncommentstrip.NewReader(strings.NewReader(s)))
		if err != nil {
			return nil, err
		}

		var config map[string]interface{}
		if err := json.Unmarshal(stripped, &config); err != nil {
			return nil, err
		}
		for name, value := range config {
			// See if we should deeply merge this field.
			if _, ok := deeplyMergedConfigFields[name]; ok {
				if mv, ok := merged[name].([]interface{}); merged[name] == nil || ok {
					if cv, ok := value.([]interface{}); merged[name] != nil || (value != nil && ok) {
						merged[name] = append(mv, cv...)
						continue
					}
				}
			}

			// Otherwise clobber any existing value.
			merged[name] = value
		}
	}
	return json.Marshal(merged)
}

func (rootResolver) Configuration() *configurationCascadeResolver {
	return &configurationCascadeResolver{}
}
