// Package docker holds static guards for the repository Dockerfile and its
// build context (.dockerignore).
package docker

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// swrRegistry is the only registry Dockerfile base images may come from.
// Direct pulls from Docker Hub / ghcr are not allowed for this build.
const swrRegistry = "swr.cn-east-3.myhuaweicloud.com/risecloud/"

// repoRoot is the repository root relative to this package directory.
const repoRoot = "../.."

// dockerfileInstructions returns the Dockerfile as logical instructions:
// comments dropped, backslash continuations joined.
func dockerfileInstructions(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot, "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	var out []string
	var cur strings.Builder
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasSuffix(trimmed, "\\") {
			cur.WriteString(strings.TrimSuffix(trimmed, "\\"))
			cur.WriteString(" ")
			continue
		}
		cur.WriteString(trimmed)
		out = append(out, cur.String())
		cur.Reset()
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// fromStage is one parsed FROM instruction.
type fromStage struct {
	image string // ARG-expanded image or earlier stage name
	name  string // AS <name>, empty when unnamed
}

func dockerfileStages(t *testing.T, ins []string) []fromStage {
	t.Helper()
	args := map[string]string{}
	argRe := regexp.MustCompile(`^ARG\s+([A-Za-z_][A-Za-z0-9_]*)=(\S+)$`)
	var stages []fromStage
	for _, in := range ins {
		if m := argRe.FindStringSubmatch(in); m != nil && len(stages) == 0 {
			// Only global ARGs (before the first FROM) apply to FROM lines.
			args[m[1]] = m[2]
			continue
		}
		fields := strings.Fields(in)
		if len(fields) < 2 || !strings.EqualFold(fields[0], "FROM") {
			continue
		}
		var rest []string
		for _, f := range fields[1:] {
			if !strings.HasPrefix(f, "--") {
				rest = append(rest, f)
			}
		}
		st := fromStage{image: os.Expand(rest[0], func(k string) string { return args[k] })}
		if len(rest) == 3 && strings.EqualFold(rest[1], "AS") {
			st.name = rest[2]
		}
		stages = append(stages, st)
	}
	if len(stages) == 0 {
		t.Fatal("Dockerfile has no FROM instruction")
	}
	return stages
}

// TestDockerfileBaseImagesComeFromSWR guards that every FROM resolves to the
// SWR risecloud namespace (directly or through the SWR ARG default), or to an
// earlier stage of the same Dockerfile.
func TestDockerfileBaseImagesComeFromSWR(t *testing.T) {
	seen := map[string]bool{}
	for _, st := range dockerfileStages(t, dockerfileInstructions(t)) {
		if !seen[st.image] && !strings.HasPrefix(st.image, swrRegistry) {
			t.Errorf("FROM %q does not come from %s", st.image, swrRegistry)
		}
		if st.name != "" {
			seen[st.name] = true
		}
	}
}

// TestDockerignoreKeepsCredentialsOutOfContext guards that credential-like
// files in the working tree never reach the build context: the builder stage
// runs `COPY . .`, and docker does not honour .gitignore.
func TestDockerignoreKeepsCredentialsOutOfContext(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot, ".dockerignore"))
	if err != nil {
		t.Fatalf("read .dockerignore: %v", err)
	}
	patterns := map[string]bool{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			patterns[line] = true
		}
	}
	for _, want := range []string{
		".git",
		"**/.ssh",
		"**/*.key",
		"**/*.pem",
		"**/id_rsa*",
		"**/id_ed25519*",
		"**/.env*",
	} {
		if !patterns[want] {
			t.Errorf(".dockerignore does not exclude %q", want)
		}
	}
}
