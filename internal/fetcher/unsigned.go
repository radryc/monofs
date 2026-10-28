package fetcher

import (
	"context"
	"io"
)

// unsignedCommit disables go-git's automatic commit signing. MonoFS creates
// commits as part of its own workflow, so commits must not depend on the
// host's commit.gpgSign setting or a registered ObjectSigner plugin.
type unsignedCommit struct{}

func (unsignedCommit) Sign(context.Context, io.Reader) ([]byte, error) {
	return nil, nil
}
