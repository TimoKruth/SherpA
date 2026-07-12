package audit

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"sherpa/internal/registry/content"
	"sherpa/internal/registry/store"
)

type Store interface {
	AllVersionRefs(ctx context.Context) ([]store.VersionRef, error)
}

type ContentStore interface {
	TagCommit(owner, name, tag string) (string, error)
	ListRepositories() ([]content.RepositoryRef, error)
	ListTags(owner, name string) ([]string, error)
}

type Report struct {
	MissingContent []store.VersionRef
	ExtraTags      []string
}

func Run(ctx context.Context, st Store, cs ContentStore) (Report, error) {
	refs, err := st.AllVersionRefs(ctx)
	if err != nil {
		return Report{}, fmt.Errorf("list metadata versions: %w", err)
	}

	known := make(map[string]struct{}, len(refs))
	var report Report
	for _, ref := range refs {
		if err := ctx.Err(); err != nil {
			return Report{}, err
		}
		known[tagKey(ref.Owner, ref.Name, ref.GitTag)] = struct{}{}
		if _, err := cs.TagCommit(ref.Owner, ref.Name, ref.GitTag); errors.Is(err, content.ErrNotFound) {
			report.MissingContent = append(report.MissingContent, ref)
		} else if err != nil {
			return Report{}, fmt.Errorf("inspect %s/%s tag %q: %w", ref.Owner, ref.Name, ref.GitTag, err)
		}
	}

	repos, err := cs.ListRepositories()
	if err != nil {
		return Report{}, fmt.Errorf("list content repositories: %w", err)
	}
	for _, repo := range repos {
		if err := ctx.Err(); err != nil {
			return Report{}, err
		}
		tags, err := cs.ListTags(repo.Owner, repo.Name)
		if err != nil {
			return Report{}, fmt.Errorf("list tags for %s/%s: %w", repo.Owner, repo.Name, err)
		}
		for _, tag := range tags {
			if _, ok := known[tagKey(repo.Owner, repo.Name, tag)]; !ok {
				report.ExtraTags = append(report.ExtraTags, formatTag(repo.Owner, repo.Name, tag))
			}
		}
	}

	slices.SortFunc(report.MissingContent, compareVersionRef)
	slices.Sort(report.ExtraTags)
	return report, nil
}

func compareVersionRef(a, b store.VersionRef) int {
	if a.Owner != b.Owner {
		return cmpString(a.Owner, b.Owner)
	}
	if a.Name != b.Name {
		return cmpString(a.Name, b.Name)
	}
	if a.Version < b.Version {
		return -1
	}
	if a.Version > b.Version {
		return 1
	}
	return cmpString(a.GitTag, b.GitTag)
}

func cmpString(a, b string) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

func tagKey(owner, name, tag string) string {
	return owner + "\x00" + name + "\x00" + tag
}

func formatTag(owner, name, tag string) string {
	return owner + "/" + name + ":" + tag
}
