// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Script-driven tests.
// See testdata/script/README for an overview.

//go:generate go test cmd/go -v -run=TestScript/README --fixreadme

package main_test

import (
	"bufio"
	"bytes"
	
	"flag"
	"fmt"
	"go/build"
	"internal/testenv"
	"internal/txtar"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	

	"cmd/go/internal/cfg"
	"cmd/go/internal/script"
	"cmd/go/internal/vcweb/vcstest"
)

var testSum = flag.String("testsum", "", `may be tidy, listm, or listall. If set, TestScript generates a go.sum file at the beginning of each test and updates test files if they pass.`)

// TestScript runs the tests in testdata/script/*.txt.
func TestScript(t *testing.T) {
}

// initScriptState creates the initial directory structure in s for unpacking a
// cmd/go script.
func initScriptDirs(t testing.TB, s *script.State) {
	must := func(err error) {
		if err != nil {
			t.Helper()
			t.Fatal(err)
		}
	}

	work := s.Getwd()
	must(s.Setenv("WORK", work))

	must(os.MkdirAll(filepath.Join(work, "tmp"), 0777))
	must(s.Setenv(tempEnvName(), filepath.Join(work, "tmp")))

	gopath := filepath.Join(work, "gopath")
	must(s.Setenv("GOPATH", gopath))
	gopathSrc := filepath.Join(gopath, "src")
	must(os.MkdirAll(gopathSrc, 0777))
	must(s.Chdir(gopathSrc))
}

func scriptEnv(srv *vcstest.Server, srvCertFile string) ([]string, error) {
	httpURL, err := url.Parse(srv.HTTP.URL)
	if err != nil {
		return nil, err
	}
	httpsURL, err := url.Parse(srv.HTTPS.URL)
	if err != nil {
		return nil, err
	}
	version, err := goVersion()
	if err != nil {
		return nil, err
	}
	env := []string{
		pathEnvName() + "=" + testBin + string(filepath.ListSeparator) + os.Getenv(pathEnvName()),
		homeEnvName() + "=/no-home",
		"CCACHE_DISABLE=1", // ccache breaks with non-existent HOME
		"GOARCH=" + runtime.GOARCH,
		"TESTGO_GOHOSTARCH=" + goHostArch,
		"GOCACHE=" + testGOCACHE,
		"GOCOVERDIR=" + os.Getenv("GOCOVERDIR"),
		"GODEBUG=" + os.Getenv("GODEBUG"),
		"GOEXE=" + cfg.ExeSuffix,
		"GOEXPERIMENT=" + os.Getenv("GOEXPERIMENT"),
		"GOOS=" + runtime.GOOS,
		"TESTGO_GOHOSTOS=" + goHostOS,
		"GOPROXY=" + proxyURL,
		"GOPRIVATE=",
		"GOROOT=" + testGOROOT,
		"GOROOT_FINAL=" + testGOROOT_FINAL, // causes spurious rebuilds and breaks the "stale" built-in if not propagated
		"GOTRACEBACK=system",
		"TESTGO_GOROOT=" + testGOROOT,
		"TESTGO_EXE=" + testGo,
		"TESTGO_VCSTEST_HOST=" + httpURL.Host,
		"TESTGO_VCSTEST_TLS_HOST=" + httpsURL.Host,
		"TESTGO_VCSTEST_CERT=" + srvCertFile,
		"GOSUMDB=" + testSumDBVerifierKey,
		"GONOPROXY=",
		"GONOSUMDB=",
		"GOVCS=*:all",
		"devnull=" + os.DevNull,
		"goversion=" + version,
		"CMDGO_TEST_RUN_MAIN=true",
	}

	if testenv.Builder() != "" || os.Getenv("GIT_TRACE_CURL") == "1" {
		// To help diagnose https://go.dev/issue/52545,
		// enable tracing for Git HTTPS requests.
		env = append(env,
			"GIT_TRACE_CURL=1",
			"GIT_TRACE_CURL_NO_DATA=1",
			"GIT_REDACT_COOKIES=o,SSO,GSSO_Uberproxy")
	}
	if !testenv.HasExternalNetwork() {
		env = append(env, "TESTGONETWORK=panic", "TESTGOVCS=panic")
	}
	if os.Getenv("CGO_ENABLED") != "" || runtime.GOOS != goHostOS || runtime.GOARCH != goHostArch {
		// If the actual CGO_ENABLED might not match the cmd/go default, set it
		// explicitly in the environment. Otherwise, leave it unset so that we also
		// cover the default behaviors.
		env = append(env, "CGO_ENABLED="+cgoEnabled)
	}

	for _, key := range extraEnvKeys {
		if val, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+val)
		}
	}

	return env, nil
}

// goVersion returns the current Go version.
func goVersion() (string, error) {
	tags := build.Default.ReleaseTags
	version := tags[len(tags)-1]
	if !regexp.MustCompile(`^go([1-9][0-9]*)\.(0|[1-9][0-9]*)$`).MatchString(version) {
		return "", fmt.Errorf("invalid go version %q", version)
	}
	return version[2:], nil
}

var extraEnvKeys = []string{
	"SYSTEMROOT",         // must be preserved on Windows to find DLLs; golang.org/issue/25210
	"WINDIR",             // must be preserved on Windows to be able to run PowerShell command; golang.org/issue/30711
	"LD_LIBRARY_PATH",    // must be preserved on Unix systems to find shared libraries
	"LIBRARY_PATH",       // allow override of non-standard static library paths
	"C_INCLUDE_PATH",     // allow override non-standard include paths
	"CC",                 // don't lose user settings when invoking cgo
	"GO_TESTING_GOTOOLS", // for gccgo testing
	"GCCGO",              // for gccgo testing
	"GCCGOTOOLDIR",       // for gccgo testing
}

// updateSum runs 'go mod tidy', 'go list -mod=mod -m all', or
// 'go list -mod=mod all' in the test's current directory if a file named
// "go.mod" is present after the archive has been extracted. updateSum modifies
// archive and returns true if go.mod or go.sum were changed.
func updateSum(t testing.TB, e *script.Engine, s *script.State, archive *txtar.Archive) (rewrite bool) {
	gomodIdx, gosumIdx := -1, -1
	for i := range archive.Files {
		switch archive.Files[i].Name {
		case "go.mod":
			gomodIdx = i
		case "go.sum":
			gosumIdx = i
		}
	}
	if gomodIdx < 0 {
		return false
	}

	var cmd string
	switch *testSum {
	case "tidy":
		cmd = "go mod tidy"
	case "listm":
		cmd = "go list -m -mod=mod all"
	case "listall":
		cmd = "go list -mod=mod all"
	default:
		t.Fatalf(`unknown value for -testsum %q; may be "tidy", "listm", or "listall"`, *testSum)
	}

	log := new(strings.Builder)
	err := e.Execute(s, "updateSum", bufio.NewReader(strings.NewReader(cmd)), log)
	if log.Len() > 0 {
		t.Logf("%s", log)
	}
	if err != nil {
		t.Fatal(err)
	}

	newGomodData, err := os.ReadFile(s.Path("go.mod"))
	if err != nil {
		t.Fatalf("reading go.mod after -testsum: %v", err)
	}
	if !bytes.Equal(newGomodData, archive.Files[gomodIdx].Data) {
		archive.Files[gomodIdx].Data = newGomodData
		rewrite = true
	}

	newGosumData, err := os.ReadFile(s.Path("go.sum"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("reading go.sum after -testsum: %v", err)
	}
	switch {
	case os.IsNotExist(err) && gosumIdx >= 0:
		// go.sum was deleted.
		rewrite = true
		archive.Files = append(archive.Files[:gosumIdx], archive.Files[gosumIdx+1:]...)
	case err == nil && gosumIdx < 0:
		// go.sum was created.
		rewrite = true
		gosumIdx = gomodIdx + 1
		archive.Files = append(archive.Files, txtar.File{})
		copy(archive.Files[gosumIdx+1:], archive.Files[gosumIdx:])
		archive.Files[gosumIdx] = txtar.File{Name: "go.sum", Data: newGosumData}
	case err == nil && gosumIdx >= 0 && !bytes.Equal(newGosumData, archive.Files[gosumIdx].Data):
		// go.sum was changed.
		rewrite = true
		archive.Files[gosumIdx].Data = newGosumData
	}
	return rewrite
}
