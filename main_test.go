package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseAddr(t *testing.T) {
	tests := []struct {
		input    string
		wantHost string
		wantPort string
		wantOk   bool
	}{
		{":5000", "", "5000", true},
		{"0.0.0.0:5000", "0.0.0.0", "5000", true},
		{"192.168.1.1:9090", "192.168.1.1", "9090", true},
		{"localhost:3128", "localhost", "3128", true},
		{"noport", "noport", "", false},
		{"", "", "", false},
	}
	for _, tt := range tests {
		host, port, ok := parseAddr(tt.input)
		if host != tt.wantHost || port != tt.wantPort || ok != tt.wantOk {
			t.Errorf("parseAddr(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tt.input, host, port, ok, tt.wantHost, tt.wantPort, tt.wantOk)
		}
	}
}

func TestArgsDescription(t *testing.T) {
	desc := Args{}.Description()
	if desc == "" {
		t.Fatal("Description() should be non-empty")
	}
}

func TestArgsVersion(t *testing.T) {
	ver := Args{}.Version()
	if !strings.Contains(ver, "outpostimage") {
		t.Fatalf("Version() = %q, want it to contain 'outpostimage'", ver)
	}
}

// TestInitStore_Docker verifies the "docker" StoreType path succeeds when the
// docker root is an empty temp dir (all sub-paths ENOENT, Init returns nil).
func TestInitStore_Docker(t *testing.T) {
	dir := t.TempDir()
	cmd := &RegistryCmd{StoreType: "docker", DockerRoot: dir}
	store := initStore(context.Background(), cmd)
	if store == nil {
		t.Fatal("initStore should return non-nil store")
	}
}

// TestInitStore_Docker_InitFail exercises the slog.Warn path in initStore when
// DockerStore.Init fails. Creating a plain file at $root/image causes
// os.ReadFile("$root/image/overlay2/repositories.json") to return ENOTDIR (not
// ENOENT), so loadRepositories returns an error and Init fails.
func TestInitStore_Docker_InitFail(t *testing.T) {
	dir := t.TempDir()
	// Obstruct the "image" sub-path so Init cannot traverse into it.
	if err := os.WriteFile(filepath.Join(dir, "image"), []byte("not a dir"), 0644); err != nil {
		t.Fatal(err)
	}
	cmd := &RegistryCmd{StoreType: "docker", DockerRoot: dir}
	store := initStore(context.Background(), cmd)
	if store == nil {
		t.Fatal("initStore should return non-nil store even when Init fails")
	}
}

// TestInitStore_Containerd verifies the "containerd" StoreType path.
func TestInitStore_Containerd(t *testing.T) {
	dir := t.TempDir()
	cmd := &RegistryCmd{StoreType: "containerd", ContainerdRoot: dir}
	store := initStore(context.Background(), cmd)
	if store == nil {
		t.Fatal("initStore should return non-nil store")
	}
}

// TestInitStore_Containerd_InitFail exercises the slog.Warn path when
// ContainerdStore.Init fails. Creating a file at $root/io.containerd.content.v1.content
// causes scanContentStore to return ENOTDIR, making Init return an error.
func TestInitStore_Containerd_InitFail(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "io.containerd.content.v1.content"), []byte("not a dir"), 0644); err != nil {
		t.Fatal(err)
	}
	cmd := &RegistryCmd{StoreType: "containerd", ContainerdRoot: dir}
	store := initStore(context.Background(), cmd)
	if store == nil {
		t.Fatal("initStore should return non-nil store even when Init fails")
	}
}

// TestInitStore_Auto_NoStores verifies the "auto" path when no container
// runtime directories exist — DetectStores returns empty, and a default
// DockerStore is returned.
func TestInitStore_Auto_NoStores(t *testing.T) {
	dir := t.TempDir()
	cmd := &RegistryCmd{StoreType: "auto", DockerRoot: dir, ContainerdRoot: dir}
	store := initStore(context.Background(), cmd)
	if store == nil {
		t.Fatal("initStore should return a default store even when nothing is detected")
	}
}

// TestInitStore_Auto_SingleStore exercises the len(stores)==1 path in
// initStore. If the real Docker root exists and its Init succeeds, DetectStores
// will return exactly one store (docker). If neither runtime is available the
// test is skipped.
func TestInitStore_Auto_SingleStore(t *testing.T) {
	const dockerRoot = "/var/lib/docker"
	if _, err := os.Stat(dockerRoot + "/image/overlay2/imagedb"); err != nil {
		t.Skip("real docker imagedb not available")
	}
	dir := t.TempDir() // non-existent containerd root
	cmd := &RegistryCmd{StoreType: "auto", DockerRoot: dockerRoot, ContainerdRoot: dir}
	store := initStore(context.Background(), cmd)
	if store == nil {
		t.Fatal("expected non-nil store for single docker store")
	}
}

// TestInitStore_Auto_MultiStore exercises the len(stores)>1 path. Requires
// both docker and containerd stores to be available and their Init to succeed.
func TestInitStore_Auto_MultiStore(t *testing.T) {
	const (
		dockerRoot     = "/var/lib/docker"
		containerdRoot = "/var/lib/containerd"
	)
	if _, err := os.Stat(dockerRoot + "/image/overlay2/imagedb"); err != nil {
		t.Skip("real docker imagedb not available")
	}
	if _, err := os.Stat(containerdRoot + "/io.containerd.content.v1.content/blobs"); err != nil {
		t.Skip("real containerd content store not available")
	}
	cmd := &RegistryCmd{StoreType: "auto", DockerRoot: dockerRoot, ContainerdRoot: containerdRoot}
	store := initStore(context.Background(), cmd)
	if store == nil {
		t.Fatal("expected non-nil MultiStore")
	}
}

func TestParseLogLevel(t *testing.T) {
	tests := []struct {
		input string
		want  slog.Level
	}{
		{"DEBUG", slog.LevelDebug},
		{"WARN", slog.LevelWarn},
		{"ERROR", slog.LevelError},
		{"INFO", slog.LevelInfo},
		{"", slog.LevelInfo},
		{"invalid", slog.LevelInfo},
		{"debug", slog.LevelInfo}, // lowercase not matched
	}
	for _, tt := range tests {
		got := parseLogLevel(tt.input)
		if got != tt.want {
			t.Errorf("parseLogLevel(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}
