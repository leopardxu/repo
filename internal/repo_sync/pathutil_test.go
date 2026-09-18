package repo_sync

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsSafeToDelete(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	repoRoot := cwd

	tests := []struct {
		name    string
		path    string
		root    string
		wantErr bool
	}{
		{"safe path", filepath.Join(cwd, "test_dir"), repoRoot, false},
		{"unsafe parent", filepath.Dir(cwd), repoRoot, true},
		{"unsafe outside repo", filepath.Join(filepath.Dir(cwd), "other"), repoRoot, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := IsSafeToDelete(tt.path, tt.root)
			if (err != nil) != tt.wantErr {
				t.Errorf("IsSafeToDelete() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
