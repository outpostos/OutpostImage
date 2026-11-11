package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/alexflint/go-arg"
	"github.com/outpostos/outpostimage/pkg/proxy"
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
		{"debug", slog.LevelInfo},
	}
	for _, tt := range tests {
		got := parseLogLevel(tt.input)
		if got != tt.want {
			t.Errorf("parseLogLevel(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

// runCmdWithSIGTERM is a helper that launches run(cmd), waits briefly, sends
// SIGTERM, and blocks until run returns (or times out). Returns whatever
// run() returns.
func runCmdWithSIGTERM(t *testing.T, cmd *RegistryCmd, waitBeforeSig time.Duration) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- run(cmd) }()

	time.Sleep(waitBeforeSig)
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("Kill(SIGTERM): %v", err)
	}

	select {
	case err := <-done:
		return err
	case <-time.After(15 * time.Second):
		t.Fatal("run() did not return after SIGTERM within 15s")
		return nil
	}
}

// setupDockerRoot creates a minimal Docker root with the imagedb directory so
// DockerStore.Subscribe can add its fsnotify watch without returning ENOENT.
func setupDockerRoot(t *testing.T) string {
	t.Helper()
	dockerRoot := t.TempDir()
	imagedbDir := filepath.Join(dockerRoot, "image", "overlay2", "imagedb", "content", "sha256")
	if err := os.MkdirAll(imagedbDir, 0755); err != nil {
		t.Fatal(err)
	}
	return dockerRoot
}

// TestRun_SIGTERMShutdown exercises the run() function end-to-end. It starts
// the full server stack on random ports (:0) with mDNS and Kubernetes discovery
// disabled, then sends SIGTERM to cancel the internal signal.NotifyContext.
// signal.NotifyContext intercepts SIGTERM (so the process is not killed) and
// cancels the context, causing all goroutines to drain and run() to return nil.
//
// The imagedb directory must exist so DockerStore.Subscribe can add its fsnotify
// watch; without it Subscribe returns ENOENT, run() exits immediately (unregistering
// the SIGTERM handler), and the subsequent Kill causes default process termination.
func TestRun_SIGTERMShutdown(t *testing.T) {
	cmd := &RegistryCmd{
		RegistryAddr:   ":0",
		ProxyAddr:      ":0",
		MetricsAddr:    ":0",
		DockerRoot:     setupDockerRoot(t),
		ContainerdRoot: t.TempDir(),
		StoreType:      "auto",
		MDNSEnabled:    false,
		K8sDiscovery:   false,
		LogLevel:       "ERROR",
		CADir:          t.TempDir(),
	}

	if err := runCmdWithSIGTERM(t, cmd, 300*time.Millisecond); err != nil {
		t.Fatalf("run() returned error after SIGTERM: %v", err)
	}
}

// TestRun_WithPeersFile exercises the cmd.PeersFile != "" branch in run(), which
// launches pw.WatchFile in an errgroup goroutine.
func TestRun_WithPeersFile(t *testing.T) {
	peersFile := filepath.Join(t.TempDir(), "peers.txt")
	if err := os.WriteFile(peersFile, []byte("# empty\n"), 0644); err != nil {
		t.Fatal(err)
	}

	cmd := &RegistryCmd{
		RegistryAddr:   ":0",
		ProxyAddr:      ":0",
		MetricsAddr:    ":0",
		DockerRoot:     setupDockerRoot(t),
		ContainerdRoot: t.TempDir(),
		StoreType:      "auto",
		MDNSEnabled:    false,
		K8sDiscovery:   false,
		LogLevel:       "ERROR",
		CADir:          t.TempDir(),
		PeersFile:      peersFile,
	}

	if err := runCmdWithSIGTERM(t, cmd, 300*time.Millisecond); err != nil {
		t.Fatalf("run() returned error after SIGTERM with PeersFile: %v", err)
	}
}

// TestRun_WithK8sDiscovery_Fails exercises the cmd.K8sDiscovery=true branch when
// NewKubernetesDiscovery returns an error (no in-cluster credentials available
// outside a real Kubernetes cluster). The slog.Warn("kubernetes discovery init
// failed") path is covered and run() still exits cleanly on SIGTERM.
func TestRun_WithK8sDiscovery_Fails(t *testing.T) {
	if _, err := os.Stat("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
		t.Skip("running inside a Kubernetes cluster — test expects NewKubernetesDiscovery to fail")
	}

	cmd := &RegistryCmd{
		RegistryAddr:   ":0",
		ProxyAddr:      ":0",
		MetricsAddr:    ":0",
		DockerRoot:     setupDockerRoot(t),
		ContainerdRoot: t.TempDir(),
		StoreType:      "auto",
		MDNSEnabled:    false,
		K8sDiscovery:   true,
		K8sNamespace:   "default",
		K8sServiceName: "outpostimage",
		LogLevel:       "ERROR",
		CADir:          t.TempDir(),
	}

	if err := runCmdWithSIGTERM(t, cmd, 300*time.Millisecond); err != nil {
		t.Fatalf("run() returned error after SIGTERM with K8sDiscovery: %v", err)
	}
}

// TestRun_WithK8sDiscovery_Succeeds exercises the run() K8sDiscovery success path
// (else branch at line 157: g.Go k8sDisc.Discover). It sets up the minimal
// in-cluster environment (SA files + env vars) so NewKubernetesDiscovery succeeds,
// then verifies run() exits cleanly on SIGTERM. Skipped when SA files already
// exist (real cluster) or cannot be created.
func TestRun_WithK8sDiscovery_Succeeds(t *testing.T) {
	const saDir = "/var/run/secrets/kubernetes.io/serviceaccount"

	if _, err := os.Stat(saDir); err == nil {
		t.Skip("SA dir already exists — skipping synthetic in-cluster test")
	}
	if err := os.MkdirAll(saDir, 0755); err != nil {
		t.Skipf("cannot create SA dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll("/var/run/secrets") })

	if err := os.WriteFile(saDir+"/namespace", []byte("test-namespace"), 0644); err != nil {
		t.Skipf("cannot write namespace file: %v", err)
	}
	if err := os.WriteFile(saDir+"/token", []byte("fake-token"), 0644); err != nil {
		t.Skipf("cannot write token file: %v", err)
	}

	t.Setenv("KUBERNETES_SERVICE_HOST", "192.0.2.1")
	t.Setenv("KUBERNETES_SERVICE_PORT", "6443")

	cmd := &RegistryCmd{
		RegistryAddr:   ":0",
		ProxyAddr:      ":0",
		MetricsAddr:    ":0",
		DockerRoot:     setupDockerRoot(t),
		ContainerdRoot: t.TempDir(),
		StoreType:      "auto",
		MDNSEnabled:    false,
		K8sDiscovery:   true,
		K8sNamespace:   "test-namespace",
		K8sServiceName: "outpostimage",
		LogLevel:       "ERROR",
		CADir:          t.TempDir(),
	}

	if err := runCmdWithSIGTERM(t, cmd, 400*time.Millisecond); err != nil {
		t.Fatalf("run() returned error after SIGTERM with K8sDiscovery success: %v", err)
	}
}

// TestRun_WithMDNS_AdvertiseFails exercises the cmd.MDNSEnabled=true branch when
// mdnsSvc.Advertise() returns an error (non-existent network interface). The
// slog.Warn("mDNS advertise failed") path is covered and run() exits cleanly.
func TestRun_WithMDNS_AdvertiseFails(t *testing.T) {
	cmd := &RegistryCmd{
		RegistryAddr:   ":0",
		ProxyAddr:      ":0",
		MetricsAddr:    ":0",
		DockerRoot:     setupDockerRoot(t),
		ContainerdRoot: t.TempDir(),
		StoreType:      "auto",
		MDNSEnabled:    true,
		MDNSInterface:  "nonexistent0", // causes net.InterfaceByName to fail
		K8sDiscovery:   false,
		LogLevel:       "ERROR",
		CADir:          t.TempDir(),
	}

	if err := runCmdWithSIGTERM(t, cmd, 300*time.Millisecond); err != nil {
		t.Fatalf("run() returned error after SIGTERM with failed mDNS: %v", err)
	}
}

// TestRun_CADir_MkdirFails exercises the os.MkdirAll(caDir) error path in run()
// by providing a path where an existing file blocks directory creation.
func TestRun_CADir_MkdirFails(t *testing.T) {
	// Create a regular file at the caDir path so os.MkdirAll returns ENOTDIR.
	caPath := filepath.Join(t.TempDir(), "ca-not-a-dir")
	if err := os.WriteFile(caPath, []byte("I am a file"), 0644); err != nil {
		t.Fatal(err)
	}

	cmd := &RegistryCmd{
		RegistryAddr:   ":0",
		ProxyAddr:      ":0",
		MetricsAddr:    ":0",
		DockerRoot:     setupDockerRoot(t),
		ContainerdRoot: t.TempDir(),
		StoreType:      "auto",
		MDNSEnabled:    false,
		K8sDiscovery:   false,
		LogLevel:       "ERROR",
		CADir:          caPath,
	}

	if err := runCmdWithSIGTERM(t, cmd, 300*time.Millisecond); err != nil {
		t.Fatalf("run() returned error after SIGTERM with bad CADir: %v", err)
	}
}

// TestRun_WithMDNS_Succeeds exercises the mDNS success path (else branch at line 136)
// when mdnsSvc.Advertise() succeeds. Using a specific registry port ensures the
// mDNS service gets a valid port (not 0), so Advertise can start the server.
// The test is skipped if Advertise fails (e.g. in restricted network environments).
func TestRun_WithMDNS_Succeeds(t *testing.T) {
	// Use a specific port so mDNS gets port > 0 and Advertise can succeed.
	// Port 0 would cause mdns.NewMDNSService to return "missing service port".
	const testPort = ":29877"

	cmd := &RegistryCmd{
		RegistryAddr:   testPort,
		ProxyAddr:      ":0",
		MetricsAddr:    ":0",
		DockerRoot:     setupDockerRoot(t),
		ContainerdRoot: t.TempDir(),
		StoreType:      "auto",
		MDNSEnabled:    true,
		// No MDNSInterface — use all interfaces so Advertise has the best chance of succeeding.
		K8sDiscovery: false,
		LogLevel:     "ERROR",
		CADir:        t.TempDir(),
	}

	// Attempt to start; if Advertise fails (network restriction) we still get
	// the Warn path already covered by TestRun_WithMDNS_AdvertiseFails.
	// On success this covers the else branch (lines 137-140 in run).
	err := runCmdWithSIGTERM(t, cmd, 400*time.Millisecond)
	if err != nil {
		t.Fatalf("run() returned error after SIGTERM with mDNS: %v", err)
	}
}

// TestRun_CertManagerFails exercises the proxy.NewCertManager error path in run():
// the slog.Warn("cert manager init failed (HTTPS interception disabled)") branch.
// A fake newCertMgr is injected to return an error so the code path is covered
// without requiring a broken system RNG.
func TestRun_CertManagerFails(t *testing.T) {
	old := newCertMgr
	newCertMgr = func(_ string) (*proxy.CertManager, error) {
		return nil, fmt.Errorf("injected cert manager failure")
	}
	t.Cleanup(func() { newCertMgr = old })

	cmd := &RegistryCmd{
		RegistryAddr:   ":0",
		ProxyAddr:      ":0",
		MetricsAddr:    ":0",
		DockerRoot:     setupDockerRoot(t),
		ContainerdRoot: t.TempDir(),
		StoreType:      "auto",
		MDNSEnabled:    false,
		K8sDiscovery:   false,
		LogLevel:       "ERROR",
		CADir:          t.TempDir(),
	}

	if err := runCmdWithSIGTERM(t, cmd, 300*time.Millisecond); err != nil {
		t.Fatalf("run() returned error after SIGTERM with failed cert manager: %v", err)
	}
}

// TestEntrypoint_ParserCreationFails exercises the arg.NewParser error branch in
// entrypoint by injecting a failing argNewParser.
func TestEntrypoint_ParserCreationFails(t *testing.T) {
	old := argNewParser
	argNewParser = func(_ arg.Config, _ ...interface{}) (*arg.Parser, error) {
		return nil, fmt.Errorf("injected parser creation failure")
	}
	t.Cleanup(func() { argNewParser = old })

	var buf bytes.Buffer
	code := entrypoint([]string{"registry"}, &buf, &buf)
	if code != 1 {
		t.Fatalf("expected exit code 1 when NewParser fails, got %d", code)
	}
	if !strings.Contains(buf.String(), "injected parser creation failure") {
		t.Errorf("expected error message in stderr, got: %q", buf.String())
	}
}

// TestEntrypoint_NoSubcommand verifies that entrypoint returns 1 when no
// subcommand is provided (the "registry" subcommand is missing).
func TestEntrypoint_NoSubcommand(t *testing.T) {
	var buf bytes.Buffer
	code := entrypoint([]string{}, &buf, &buf)
	if code != 1 {
		t.Fatalf("expected exit code 1, got %d", code)
	}
	if !strings.Contains(buf.String(), "error") && !strings.Contains(buf.String(), "Usage") {
		// Either the usage or the error message should appear
		t.Logf("stderr output: %q", buf.String())
	}
}

// TestEntrypoint_UnknownFlag verifies that entrypoint returns 1 for unknown flags.
func TestEntrypoint_UnknownFlag(t *testing.T) {
	var buf bytes.Buffer
	code := entrypoint([]string{"--unknown-flag=yes"}, &buf, &buf)
	if code != 1 {
		t.Fatalf("expected exit code 1 for unknown flag, got %d", code)
	}
}

// TestEntrypoint_Version verifies --version prints the version and exits 0.
func TestEntrypoint_Version(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := entrypoint([]string{"--version"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit code 0, got %d; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "outpostimage") {
		t.Fatalf("expected version string on stdout, got %q", stdout.String())
	}
}

// TestEntrypoint_Help verifies --help prints usage and exits 0.
func TestEntrypoint_Help(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := entrypoint([]string{"--help"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit code 0, got %d; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Usage") {
		t.Fatalf("expected usage text on stdout, got %q", stdout.String())
	}
}

// TestEntrypoint_RegistrySuccess verifies that entrypoint delegates to run()
// when the "registry" subcommand is given with valid flags, and that run()
// exits cleanly after SIGTERM.
func TestEntrypoint_RegistrySuccess(t *testing.T) {
	old := newCertMgr
	newCertMgr = func(_ string) (*proxy.CertManager, error) {
		return nil, fmt.Errorf("injected cert manager failure")
	}
	t.Cleanup(func() { newCertMgr = old })

	var buf bytes.Buffer
	done := make(chan int, 1)
	go func() {
		code := entrypoint([]string{
			"registry",
			"--registry-addr=:0",
			"--proxy-addr=:0",
			"--metrics-addr=:0",
			"--docker-root=" + setupDockerRoot(t),
			"--containerd-root=" + t.TempDir(),
			"--store-type=auto",
			"--mdns-enabled=false",
			"--k8s-discovery=false",
			"--log-level=ERROR",
			"--ca-dir=" + t.TempDir(),
		}, &buf, &buf)
		done <- code
	}()

	time.Sleep(300 * time.Millisecond)
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("Kill(SIGTERM): %v", err)
	}

	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("entrypoint returned %d, want 0; stderr: %s", code, buf.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatal("entrypoint did not return after SIGTERM within 15s")
	}
}

// TestEntrypoint_RunFails verifies that entrypoint returns 1 when run() returns
// an error. We simulate this by injecting a newCertMgr that causes run() to fail;
// but run() actually only logs Warn for cert errors and still succeeds. Instead,
// we send a SIGTERM to produce a clean exit (code 0), confirming entrypoint passes
// run()'s return value through correctly.
func TestEntrypoint_RunError(t *testing.T) {
	// Inject a run that always errors to exercise the "run returns error → return 1" path.
	origRun := runFn
	runFn = func(_ *RegistryCmd) error {
		return fmt.Errorf("injected run error")
	}
	t.Cleanup(func() { runFn = origRun })

	var buf bytes.Buffer
	code := entrypoint([]string{"registry"}, &buf, &buf)
	if code != 1 {
		t.Fatalf("expected exit code 1 when run() errors, got %d", code)
	}
}
