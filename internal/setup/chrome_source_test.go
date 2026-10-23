package setup

import (
	"strings"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/chromeauth"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

func TestChromeRootFlagsKeepExplicitSourceAndSelectors(t *testing.T) {
	input := `C:\Fixture Chrome\Dedicated`
	want, err := chromeauth.NormalizeChromeRoot(input)
	if err != nil {
		t.Fatal(err)
	}
	options, err := parseSetupFlags([]string{"--chrome-root", input, "--profile", "Default", "--email", "fixture@example.com"}, config.Default())
	if err != nil {
		t.Fatal(err)
	}
	if options.chromeRoot != want || len(options.profiles) != 1 || options.profiles[0] != "Default" || len(options.emails) != 1 || options.emails[0] != "fixture@example.com" {
		t.Fatal("CLI lost root or profile/email union selectors")
	}
}

func TestChromeRootFlagsRejectAmbiguousPathsWithoutDiscovery(t *testing.T) {
	for _, root := range []string{"relative/root", "C:relative", `\\server\share\Chrome`, `C:\Dedicated\..\Daily`} {
		if _, err := parseSetupFlags([]string{"--chrome-root", root}, config.Default()); err == nil {
			t.Fatal("CLI accepted ambiguous Chrome root")
		}
	}
	options, err := parseSetupFlags(nil, config.Default())
	if err != nil {
		t.Fatal(err)
	}
	if options.chromeRoot != "" {
		t.Fatal("omitted CLI root inferred a persisted source")
	}
}

func TestChromePromptReturnsOpaqueRootScopedID(t *testing.T) {
	accounts := []chromeauth.Account{{ID: "chrome-fixture-a/Default/111", Profile: "Default", Email: "fixture@example.com", Importable: true}, {ID: "chrome-fixture-b/Default/111", Profile: "Default", Email: "other@example.com", Importable: false}}
	var output strings.Builder
	selected, err := promptChromeAccounts(accounts, strings.NewReader("1\n"), &output)
	if err != nil || len(selected) != 1 || selected[0] != accounts[0].ID {
		t.Fatal("CLI rewrote the root-scoped selection")
	}
}
