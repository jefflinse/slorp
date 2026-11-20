package opencode

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNativeBundledPluginInstallAndUnload(t *testing.T) {
	if os.Getenv("SLORP_NATIVE_TEST") != "1" {
		t.Skip("set SLORP_NATIVE_TEST=1 to test the installed OpenCode in an isolated environment")
	}
	bin, err := exec.LookPath("opencode")
	if err != nil {
		t.Fatal(err)
	}
	for _, replacement := range []bool{false, true} {
		name := "default"
		if replacement {
			name = "replacement"
		}
		t.Run(name, func(t *testing.T) {
			nativeBundledPluginInstallAndUnload(t, bin, replacement)
		})
	}
}

func nativeBundledPluginInstallAndUnload(t *testing.T, bin string, replacement bool) {
	a := fixture(t)
	a.Bin = bin
	if replacement {
		a.ExtraConfigDir = filepath.Join(a.Home, "replacement")
		put(t, filepath.Join(a.ConfigDir, "opencode.json"), "{inactive configuration}")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if err := a.Install(ctx); err != nil {
		t.Fatal(err)
	}
	cmd := a.command(ctx, "serve", "--hostname", "127.0.0.1", "--port", "0")
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + a.Home,
		"XDG_CONFIG_HOME=" + filepath.Dir(a.ConfigDir),
		"XDG_DATA_HOME=" + filepath.Join(a.Home, "data"),
		"XDG_STATE_HOME=" + filepath.Join(a.Home, "state"),
		"XDG_CACHE_HOME=" + filepath.Join(a.Home, "cache"),
		"OPENCODE_PASSWORD=isolated-plugin-test",
	}
	if replacement {
		cmd.Env = append(cmd.Env, "OPENCODE_CONFIG_DIR="+a.ExtraConfigDir)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Interrupt)
		cancel()
		_ = cmd.Wait()
		if t.Failed() {
			t.Log(stderr.String())
		}
	})
	address := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if _, value, ok := strings.Cut(scanner.Text(), "server listening on "); ok {
				select {
				case address <- strings.TrimSpace(value):
				default:
				}
			}
		}
	}()
	var base string
	select {
	case base = <-address:
	case <-ctx.Done():
		t.Fatal("OpenCode did not start:", ctx.Err())
	}
	client := &http.Client{Timeout: 5 * time.Second}
	request := func(method, path string, dst any) {
		t.Helper()
		query := url.Values{"location[directory]": []string{a.Home}}
		req, err := http.NewRequestWithContext(ctx, method, base+path+"?"+query.Encode(), nil)
		if err != nil {
			t.Fatal(err)
		}
		req.SetBasicAuth("opencode", "isolated-plugin-test")
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode < 200 || res.StatusCode >= 300 {
			b, _ := io.ReadAll(res.Body)
			t.Fatalf("%s %s: %d %s", method, path, res.StatusCode, b)
		}
		if dst != nil {
			if err := json.NewDecoder(res.Body).Decode(dst); err != nil {
				t.Fatal(err)
			}
		}
	}
	wait := func(installed bool) {
		t.Helper()
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			var result struct {
				Data []struct {
					ID    string          `json:"id"`
					State json.RawMessage `json:"state"`
				} `json:"data"`
			}
			request(http.MethodGet, "/api/plugin", &result)
			found := false
			for _, plugin := range result.Data {
				if plugin.ID == "slorp" {
					found = true
					if strings.Contains(string(plugin.State), `"error"`) {
						t.Fatalf("installed plugin failed: %s", plugin.State)
					}
				}
			}
			if found == installed {
				return
			}
			select {
			case <-ticker.C:
			case <-ctx.Done():
				t.Fatalf("plugin installed=%t never observed: %v", installed, ctx.Err())
			}
		}
	}
	wait(true)
	if err := a.Uninstall(ctx); err != nil {
		t.Fatal(err)
	}
	request(http.MethodPost, "/api/location/reload", nil)
	wait(false)
}
