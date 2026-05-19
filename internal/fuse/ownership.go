package fuse

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fuse"
)

type nodeOwner struct {
	uid uint32
	gid uint32
}

func currentProcessOwner() nodeOwner {
	return nodeOwner{uid: uint32(os.Getuid()), gid: uint32(os.Getgid())}
}

func ResolvePathOwner(path string) (uint32, uint32, error) {
	owner, err := ownerFromPath(path)
	if err != nil {
		return 0, 0, err
	}
	return owner.uid, owner.gid, nil
}

func ownerFromPath(path string) (nodeOwner, error) {
	resolved, err := nearestExistingPath(path)
	if err != nil {
		return nodeOwner{}, err
	}
	return statPathOwner(resolved)
}

func nearestExistingPath(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("path is empty")
	}

	current := filepath.Clean(path)
	for {
		if _, err := os.Stat(current); err == nil {
			return current, nil
		} else if !os.IsNotExist(err) {
			return "", fmt.Errorf("stat %q: %w", current, err)
		}

		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("no existing ancestor for %q", path)
		}
		current = parent
	}
}

func statPathOwner(path string) (nodeOwner, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nodeOwner{}, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nodeOwner{}, fmt.Errorf("stat %q did not expose uid/gid", path)
	}
	return nodeOwner{uid: stat.Uid, gid: stat.Gid}, nil
}

func ensurePathOwner(path string, owner nodeOwner) error {
	current, err := statPathOwner(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if current == owner {
		return nil
	}
	if os.Geteuid() != 0 {
		return nil
	}
	if err := os.Chown(path, int(owner.uid), int(owner.gid)); err != nil {
		return fmt.Errorf("chown %q: %w", path, err)
	}
	return nil
}

func (n *MonoNode) ownerIDs() (uint32, uint32) {
	if n == nil || (n.owner.uid == 0 && n.owner.gid == 0) {
		owner := currentProcessOwner()
		return owner.uid, owner.gid
	}
	return n.owner.uid, n.owner.gid
}

func (n *MonoNode) setAttrOwner(out *fuse.AttrOut) {
	if out == nil {
		return
	}
	out.Uid, out.Gid = n.ownerIDs()
}

func (n *MonoNode) setEntryOwner(out *fuse.EntryOut) {
	if out == nil {
		return
	}
	out.Uid, out.Gid = n.ownerIDs()
}

func (n *MonoNode) SetVisibleOwner(uid, gid uint32) {
	if n == nil {
		return
	}
	n.owner = nodeOwner{uid: uid, gid: gid}
	if projection, ok := n.workspaceGit.(*WorkspaceGitProjection); ok && projection != nil {
		projection.SetOwner(uid, gid)
	}
}
