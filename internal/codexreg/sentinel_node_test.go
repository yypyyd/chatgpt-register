package codexreg

import (
	"context"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestSentinelNodeNativeSelftest(t *testing.T) {
	bin := sentinelNodeBin()
	if bin == "" {
		t.Skip("未安装 node")
	}
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller")
	}
	script := filepath.Join(filepath.Dir(thisFile), "..", "..", "scripts", "sentinel_node.js")
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, script, "--selftest")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("selftest: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "selftest_ok") {
		t.Fatalf("output=%s", out)
	}
}

func TestChromeUserAgentIsNotZeroBuild(t *testing.T) {
	ua := chromeUserAgent("146")
	if strings.Contains(ua, "Chrome/146.0.0.0") {
		t.Fatalf("UA still uses dummy build: %s", ua)
	}
}
