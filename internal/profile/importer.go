package profile

import "sherpa/internal/gitutil"

func git(dir string, args ...string) error {
	_, err := gitutil.Run(dir, args...)
	return err
}
