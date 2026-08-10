package contract_test

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
)

type listedPackage struct {
	ImportPath string
}

func TestGuestImportClosure(t *testing.T) {
	command := exec.Command("go", "list", "-deps", "-json", "github.com/srimajji/dsx/cmd/dsx-guest")
	command.Env = append(os.Environ(), "GOOS=linux", "GOARCH=arm64", "CGO_ENABLED=0")
	output, err := command.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(output)))
	for {
		var pkg listedPackage
		if err := decoder.Decode(&pkg); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("decode go list: %v", err)
		}
		for _, forbidden := range []string{
			"github.com/srimajji/dsx/internal/hostcmd",
			"github.com/srimajji/dsx/internal/runtime/apple",
			"github.com/charmbracelet/",
		} {
			if strings.HasPrefix(pkg.ImportPath, forbidden) {
				t.Fatalf("guest imports forbidden package %q", pkg.ImportPath)
			}
		}
	}
}
