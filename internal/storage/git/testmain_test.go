package git

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "monofs-storage-git-test-home-*")
	if err != nil {
		panic(err)
	}
	oldHome, hadHome := os.LookupEnv("HOME")
	oldXDG, hadXDG := os.LookupEnv("XDG_CONFIG_HOME")
	_ = os.Setenv("HOME", home)
	_ = os.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	code := m.Run()
	if hadHome {
		_ = os.Setenv("HOME", oldHome)
	} else {
		_ = os.Unsetenv("HOME")
	}
	if hadXDG {
		_ = os.Setenv("XDG_CONFIG_HOME", oldXDG)
	} else {
		_ = os.Unsetenv("XDG_CONFIG_HOME")
	}
	_ = os.RemoveAll(home)
	os.Exit(code)
}
