package bootstrapcred

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckPasswordPolicy(t *testing.T) {
	tests := []struct {
		name    string
		pw      string
		wantErr bool
	}{
		{"public default", "admin", true},
		{"empty", "", true},
		{"one short of the minimum", strings.Repeat("a", MinPasswordLength-1), true},
		{"exactly the minimum", strings.Repeat("a", MinPasswordLength), false},
		{"long", strings.Repeat("b", 64), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckPasswordPolicy(tt.pw)

			if (err != nil) != tt.wantErr {
				t.Fatalf("CheckPasswordPolicy(%q) = %v, wantErr %v", tt.pw, err, tt.wantErr)
			}
			if err != nil && tt.pw != "" && tt.pw != DefaultPassword && strings.Contains(err.Error(), tt.pw) {
				t.Error("the policy error leaks the password")
			}
		})
	}
}

func TestReadSafe_EmptyFileIsReportedAsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	if err := WriteNew(path, "x"); err != nil {
		t.Fatalf("WriteNew: %v", err)
	}
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}

	_, err := ReadSafe(path)

	if !errors.Is(err, ErrEmptySecretFile) {
		t.Errorf("ReadSafe(empty) = %v, want ErrEmptySecretFile", err)
	}
}

func TestWriteNew_FailsWithoutLeakingTheSecret(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no", "such", "dir", FileName)

	err := WriteNew(path, "secret-value-123")

	if err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("WriteNew = %v, want an error naming the path", err)
	}
	if strings.Contains(err.Error(), "secret-value-123") {
		t.Error("the error leaks the secret")
	}
}
