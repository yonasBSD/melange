// Copyright 2024 Chainguard, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:generate go run ./../../ build --generate-index=false --out-dir=./testdata/generated ./testdata/ld-so-conf-d-test.yaml --arch=x86_64
//go:generate go run ./../../ build --generate-index=false --out-dir=./testdata/generated ./testdata/shbang-test.yaml --arch=x86_64
//go:generate go run ./../../ build --generate-index=false --source-dir=./testdata/go-fips-bin/ --out-dir=./testdata/generated ./testdata/go-fips-bin/go-fips-bin.yaml --arch=x86_64
//go:generate curl -s -o ./testdata/py3-seaborn.yaml https://raw.githubusercontent.com/wolfi-dev/os/7a39ac1d0603a3561790ea2201dd8ad7c2b7e51e/py3-seaborn.yaml
//go:generate curl -s -o ./testdata/systemd.yaml https://raw.githubusercontent.com/wolfi-dev/os/7a39ac1d0603a3561790ea2201dd8ad7c2b7e51e/systemd.yaml

package sca

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"chainguard.dev/apko/pkg/apk/apk"
	"chainguard.dev/apko/pkg/apk/expandapk"
	"chainguard.dev/melange/pkg/config"
	"github.com/chainguard-dev/clog/slogtest"
	"github.com/google/go-cmp/cmp"
	"gopkg.in/ini.v1"
)

type testHandle struct {
	pkg apk.Package
	exp *expandapk.APKExpanded
	cfg *config.Configuration
}

func (th *testHandle) PackageName() string {
	return th.pkg.Name
}

func (th *testHandle) Version() string {
	return th.pkg.Version
}

func (th *testHandle) RelativeNames() []string {
	// TODO: Support subpackages?
	return []string{th.pkg.Name}
}

func (th *testHandle) FilesystemForRelative(pkgName string) (SCAFS, error) {
	if pkgName != th.PackageName() {
		return nil, fmt.Errorf("TODO: implement FilesystemForRelative, %q != %q", pkgName, th.PackageName())
	}

	return th.exp.TarFS, nil
}

func (th *testHandle) Filesystem() (SCAFS, error) {
	return th.exp.TarFS, nil
}

func (th *testHandle) Options() config.PackageOption {
	if th.cfg.Package.Options == nil {
		return config.PackageOption{}
	}
	return *th.cfg.Package.Options
}

func (th *testHandle) BaseDependencies() config.Dependencies {
	return th.cfg.Package.Dependencies
}

func (th *testHandle) InstalledPackages() map[string]string {
	return map[string]string{}
}

func (th *testHandle) PkgResolver() *apk.PkgResolver {
	return nil
}

// TODO: Loose coupling.
func handleFromApk(ctx context.Context, t *testing.T, apkfile, melangefile string) *testHandle {
	t.Helper()
	var file io.Reader
	if strings.HasPrefix(apkfile, "https://") {
		resp, err := http.Get(apkfile)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		file = resp.Body
	} else {
		var err error
		file, err = os.Open(filepath.Join("testdata", apkfile))
		if err != nil {
			t.Fatal(err)
		}
	}

	exp, err := expandapk.ExpandApk(ctx, file, "")
	if err != nil {
		t.Fatal(err)
	}

	// Get the package name
	info, err := exp.ControlFS.Open(".PKGINFO")
	if err != nil {
		t.Fatal(err)
	}
	defer info.Close()

	cfg, err := ini.ShadowLoad(info)
	if err != nil {
		t.Fatal(err)
	}

	var pkg apk.Package
	if err = cfg.MapTo(&pkg); err != nil {
		t.Fatal(err)
	}
	pkg.BuildTime = time.Unix(pkg.BuildDate, 0).UTC()
	pkg.InstalledSize = pkg.Size
	pkg.Size = uint64(exp.Size)
	pkg.Checksum = exp.ControlHash

	pkgcfg, err := config.ParseConfiguration(ctx, filepath.Join("testdata", melangefile))
	if err != nil {
		t.Fatal(err)
	}

	return &testHandle{
		pkg: pkg,
		exp: exp,
		cfg: pkgcfg,
	}
}

func TestExecableSharedObjects(t *testing.T) {
	ctx := slogtest.Context(t)
	th := handleFromApk(ctx, t, "libcap-2.69-r0.apk", "libcap.yaml")
	defer th.exp.Close()

	got := config.Dependencies{}
	if err := Analyze(ctx, th, &got); err != nil {
		t.Fatal(err)
	}

	want := config.Dependencies{
		Runtime: []string{
			"so:ld-linux-aarch64.so.1",
			"so:libc.so.6",
			"so:libcap.so.2",
			"so:libpsx.so.2",
		},
		Provides: []string{
			"so-ver:libcap.so.2=2.69-r0",
			"so-ver:libpsx.so.2=2.69-r0",
			"so:libcap.so.2=2",
			"so:libpsx.so.2=2",
		},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Analyze(): (-want, +got):\n%s", diff)
	}
}

func TestVendoredPkgConfig(t *testing.T) {
	ctx := slogtest.Context(t)
	// Generated by:
	// curl -L https://packages.wolfi.dev/os/aarch64/neon-4604-r0.apk > neon.apk
	// tardegrade <neon.apk echo $(tar -tf neon.apk| head -n 2) $(tar -tf neon.apk | grep pkgconfig) usr/libexec/neon/v14/lib/libecpg_compat.so.3.14 > neon-4604-r0.apk
	th := handleFromApk(ctx, t, "neon-4604-r0.apk", "neon.yaml")
	defer th.exp.Close()

	got := config.Dependencies{}
	if err := Analyze(ctx, th, &got); err != nil {
		t.Fatal(err)
	}

	want := config.Dependencies{
		Runtime: []string{
			// We only include libecpg_compat.so.3 to test that "libexec" isn't treated as a library directory.
			// These are dependencies of libecpg_compat.so.3, but if we had the whole neon APK it would look different.
			"so:ld-linux-aarch64.so.1",
			"so:libc.so.6",
			"so:libecpg.so.6",
			"so:libpgtypes.so.3",
			"so:libpq.so.5",
		},
		Vendored: []string{
			"pc:libecpg=4604-r0",
			"pc:libecpg_compat=4604-r0",
			"pc:libpgtypes=4604-r0",
			"pc:libpq=4604-r0",
			"so-ver:libecpg_compat.so.3=4604-r0",
			"so:libecpg_compat.so.3=3",
		},
	}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Analyze(): (-want, +got):\n%s", diff)
	}
}

func TestRubySca(t *testing.T) {
	ctx := slogtest.Context(t)
	// Generated by:
	// wget https://packages.wolfi.dev/os/x86_64/ruby3.2-base64-0.2.0-r2.apk
	th := handleFromApk(ctx, t, "ruby3.2-base64-0.2.0-r2.apk", "ruby3.2-base64.yaml")
	defer th.exp.Close()

	got := config.Dependencies{}
	if err := Analyze(ctx, th, &got); err != nil {
		t.Fatal(err)
	}

	want := config.Dependencies{Runtime: []string{"ruby-3.2"}}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Analyze(): (-want, +got):\n%s", diff)
	}
}

func TestDocSca(t *testing.T) {
	ctx := slogtest.Context(t)
	// Generated by:
	// wget https://packages.wolfi.dev/os/x86_64/bash-doc-5.2.37-r2.apk
	th := handleFromApk(ctx, t, "bash-doc-5.2.37-r2.apk", "bash.yaml")
	defer th.exp.Close()

	got := config.Dependencies{}
	if err := Analyze(ctx, th, &got); err != nil {
		t.Fatal(err)
	}

	want := config.Dependencies{Runtime: []string{"man-db", "texinfo"}}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Analyze(): (-want, +got):\n%s", diff)
	}
}

func TestUnstableSonames(t *testing.T) {
	ctx := slogtest.Context(t)
	// Generated by:
	// curl -L https://packages.wolfi.dev/os/aarch64/aws-c-s3-0.4.9-r0.apk > aws.apk
	// tardegrade <aws.apk echo $(tar -tf aws.apk| head -n 6) > aws-c-s3-0.4.9-r0.apk
	th := handleFromApk(ctx, t, "aws-c-s3-0.4.9-r0.apk", "aws-c-s3.yaml")
	defer th.exp.Close()

	got := config.Dependencies{}
	if err := Analyze(ctx, th, &got); err != nil {
		t.Fatal(err)
	}

	want := config.Dependencies{
		Runtime: []string{
			"so:ld-linux-aarch64.so.1",
			"so:libaws-c-auth.so.1.0.0",
			"so:libaws-c-cal.so.1.0.0",
			"so:libaws-c-common.so.1",
			"so:libaws-c-http.so.1.0.0",
			"so:libaws-c-io.so.1.0.0",
			"so:libaws-c-s3.so.0unstable",
			"so:libaws-checksums.so.1.0.0",
			"so:libc.so.6",
		},
		Provides: []string{
			"so-ver:libaws-c-s3.so.0unstable=0.4.9-r0",
			"so:libaws-c-s3.so.0unstable=0",
		},
	}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Analyze(): (-want, +got):\n%s", diff)
	}
}

func TestShbangDeps(t *testing.T) {
	ctx := slogtest.Context(t)
	// Generated with `go generate ./...`
	th := handleFromApk(ctx, t, "generated/x86_64/shbang-test-1-r1.apk", "shbang-test.yaml")
	defer th.exp.Close()

	want := config.Dependencies{
		Runtime: []string{
			"cmd:bash",
			"cmd:envDashSCmd",
			"cmd:python3.12",
			"so:ld-linux-x86-64.so.2",
			"so:libc.so.6",
		},
		Provides: nil,
	}

	got := config.Dependencies{}
	if err := Analyze(ctx, th, &got); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Analyze(): (-want, +got):\n%s", diff)
	}
}

func TestGetShbang(t *testing.T) {
	for i, td := range []struct {
		content string
		want    string
		wantErr string
	}{
		{"#!/usr/bin/env bash\n", "bash", ""},
		{"#!/usr/bin/env python3.12\nwith open...\n", "python3.12", ""},
		// /bin/sh is explicitly ignored.
		{"#!/bin/sh\necho hi world\n", "", ""},
		{"#!/bin/dash\necho hi world\n", "/bin/dash", ""},
		{"#!/usr/bin/env -S bash -x\necho hi world\n", "bash", ""},
		{"#!/usr/bin/env bash -x\necho hi world\n", "bash", "multiple arguments"},
		{"cs101 assignment", "", ""},
		// no carriage return in file
		{"#!/usr/bin/perl", "/usr/bin/perl", ""},
	} {
		got, gotErr := getShbang(bytes.NewReader([]byte(td.content)))
		if td.wantErr != "" {
			if gotErr == nil {
				t.Errorf("%d - expected err, got %s", i, got)
			} else if matched, err := regexp.MatchString(td.wantErr, fmt.Sprintf("%v", gotErr)); err != nil {
				t.Errorf("%d - bad test, failed regexp.Match(%s)", i, td.wantErr)
			} else if !matched {
				t.Errorf("%d - expected err '%s', got '%s'", i, td.wantErr, gotErr)
			}
		} else {
			if gotErr != nil {
				t.Errorf("%d - unexpected err %v", i, gotErr)
				continue
			}
			if td.want != got {
				t.Errorf("%d - got %d '%s', expected %d '%s'", i, len(got), got, len(td.want), td.want)
			}
		}
	}
}

func TestLdSoConfD(t *testing.T) {
	ctx := slogtest.Context(t)
	// Generated with `go generate ./...`
	th := handleFromApk(ctx, t, "generated/x86_64/ld-so-conf-d-test-1-r1.apk", "ld-so-conf-d-test.yaml")
	defer th.exp.Close()

	if extraLibPaths, err := getLdSoConfDLibPaths(ctx, th); err != nil {
		t.Fatal(err)
	} else if extraLibPaths == nil {
		t.Error("getLdSoConfDLibPaths: expected 'my/lib/test', got nil")
	} else {
		if extraLibPaths[0] != "my/lib/test" {
			t.Errorf("getLdSoConfDLibPaths: expected 'my/lib/test', got '%s'", extraLibPaths[0])
		}
	}
}
