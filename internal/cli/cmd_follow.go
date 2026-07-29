package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"sherpa/internal/state"
)

const maxPendingFollowSync = 50

func init() {
	register("follow", cmdFollow)
	register("unfollow", cmdUnfollow)
}

func cmdFollow(ctx *Ctx, args []string) error {
	owner, name, err := parseRegistryStackRefArgs(args, "follow")
	if err != nil {
		return err
	}
	base, client, err := socialClientFromEnvironment(ctx.Home)
	if err != nil {
		return err
	}
	retryPendingFollows(ctx, base, client)
	follow, err := client.Follow(context.Background(), owner, name)
	if err != nil {
		return err
	}
	fmt.Fprintf(ctx.Stdout, "following @%s/%s at v%d\n", owner, name, follow.LatestVersion)
	return nil
}

func cmdUnfollow(ctx *Ctx, args []string) error {
	owner, name, err := parseRegistryStackRefArgs(args, "unfollow")
	if err != nil {
		return err
	}
	base, client, err := socialClientFromEnvironment(ctx.Home)
	if err != nil {
		return err
	}
	retryPendingFollows(ctx, base, client)
	if err := client.Unfollow(context.Background(), owner, name); err != nil {
		return err
	}
	removePendingFollow(ctx.Home, base, "@"+owner+"/"+name)
	fmt.Fprintf(ctx.Stdout, "unfollowed @%s/%s\n", owner, name)
	return nil
}

func parseRegistryStackRefArgs(args []string, command string) (string, string, error) {
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		return "", "", fmt.Errorf("usage: sherpa %s @owner/name", command)
	}
	owner, name, ok := parseRegistryStackRef(args[0])
	if !ok {
		return "", "", errors.New("registry ref must be @owner/name")
	}
	return owner, name, nil
}

func socialClientFromEnvironment(home string) (string, *registrySocialClient, error) {
	base, err := normalizeRegistryBase(registryBaseURL())
	if err != nil {
		return "", nil, err
	}
	session, err := registryUserSession(home, base)
	if err != nil {
		return "", nil, err
	}
	client, err := newRegistrySocialClient(base, session.AccessToken)
	if err != nil {
		return "", nil, err
	}
	return base, client, nil
}

func retryPendingFollows(ctx *Ctx, base string, client *registrySocialClient) {
	st, err := state.Load(ctx.Home)
	if err != nil {
		fmt.Fprintf(ctx.Stderr, "warning: could not load pending follows: %v\n", err)
		return
	}
	registry := st.Registries[base]
	if len(registry.PendingFollows) == 0 {
		return
	}
	remaining := append([]string(nil), registry.PendingFollows...)
	attempts := len(remaining)
	if attempts > maxPendingFollowSync {
		attempts = maxPendingFollowSync
	}
	changed := false
	for _, ref := range remaining[:attempts] {
		owner, name, ok := parseRegistryStackRef(ref)
		if !ok {
			registry.PendingFollows = withoutString(registry.PendingFollows, ref)
			changed = true
			continue
		}
		if _, err := client.Follow(context.Background(), owner, name); err != nil {
			continue
		}
		registry.PendingFollows = withoutString(registry.PendingFollows, ref)
		changed = true
	}
	if !changed {
		return
	}
	st.Registries[base] = registry
	if err := st.Save(ctx.Home); err != nil {
		fmt.Fprintf(ctx.Stderr, "warning: could not save pending follows: %v\n", err)
	}
}

func enqueueAndAttemptFollow(ctx *Ctx, origin *state.RegistryOrigin) {
	if origin == nil {
		return
	}
	ref := "@" + origin.Owner + "/" + origin.Stack
	st, err := state.Load(ctx.Home)
	if err != nil {
		fmt.Fprintf(ctx.Stderr, "warning: could not queue follow for %s\n", ref)
		return
	}
	registry := st.Registries[origin.RegistryURL]
	registry.PendingFollows = append(registry.PendingFollows, ref)
	st.Registries[origin.RegistryURL] = registry
	if err := st.Save(ctx.Home); err != nil {
		fmt.Fprintf(ctx.Stderr, "warning: could not queue follow for %s\n", ref)
		return
	}
	session, err := registryUserSession(ctx.Home, origin.RegistryURL)
	if err != nil {
		fmt.Fprintf(ctx.Stderr, "warning: follow for %s is queued; run `sherpa login` to sync it\n", ref)
		return
	}
	client, err := newRegistrySocialClient(origin.RegistryURL, session.AccessToken)
	if err != nil {
		fmt.Fprintf(ctx.Stderr, "warning: follow for %s remains queued\n", ref)
		return
	}
	if _, err := client.Follow(context.Background(), origin.Owner, origin.Stack); err != nil {
		fmt.Fprintf(ctx.Stderr, "warning: follow for %s remains queued\n", ref)
		return
	}
	removePendingFollow(ctx.Home, origin.RegistryURL, ref)
}

func removePendingFollow(home, base, ref string) {
	st, err := state.Load(home)
	if err != nil {
		return
	}
	registry := st.Registries[base]
	registry.PendingFollows = withoutString(registry.PendingFollows, ref)
	st.Registries[base] = registry
	_ = st.Save(home)
}

func withoutString(values []string, target string) []string {
	result := values[:0]
	for _, value := range values {
		if value != target {
			result = append(result, value)
		}
	}
	return result
}
