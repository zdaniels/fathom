package skills

import (
	"context"
	"fmt"
	"github.com/zdaniels/fathom/internal/security"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// isolatedCommand runs offline untrusted skills in a disposable container. Only
// the runner file and the selected skill directory are mounted, both read-only.
// The Docker daemon must be a trusted local daemon. No fallback is permitted.
func (s *Sandbox) isolatedCommand(ctx context.Context, inv *SandboxInvocation, args []string) (*exec.Cmd, error) {
	docker, err := exec.LookPath("docker")
	if err != nil {
		return nil, fmt.Errorf("required skill isolation needs Docker: %w", err)
	}
	entry, err := filepath.EvalSymlinks(inv.EntryPoint)
	if err != nil {
		return nil, err
	}
	entry, err = filepath.Abs(entry)
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(entry)
	for _, protected := range s.isolation.protect {
		rel, err := filepath.Rel(dir, protected)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("skill directory contains protected data")
		}
	}
	runner := s.runnerPath
	image, bin := "node:22-slim", "node"
	dest := "/runner.js"
	if inv.Language == "python" {
		runner = s.runnerPyPath
		image, bin = "python:3.12-slim", "python3"
		dest = "/runner.py"
	}
	runner, err = filepath.Abs(runner)
	if err != nil {
		return nil, err
	}
	if strings.ContainsAny(dir+runner, ",\n\r") {
		return nil, fmt.Errorf("invalid container mount path")
	}
	inv.EntryPoint = "/skill/" + filepath.Base(entry)
	uid := os.Getuid()
	if uid == 0 {
		uid = 65532
	}
	argv := []string{"run", "--rm", "--name", "fathom-skill-" + security.GenerateID(), "-i", "--network=none", "--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges", "--pids-limit=128", "--memory=512m", "--cpus=1", "--user", strconv.Itoa(uid), "--tmpfs=/tmp:rw,nosuid,nodev,size=64m", "--env=HOME=/tmp", "--workdir=/skill", "--mount", "type=bind,src=" + dir + ",dst=/skill,readonly", "--mount", "type=bind,src=" + runner + ",dst=" + dest + ",readonly", image, bin}
	args = append([]string{}, args...)
	args[len(args)-1] = dest
	argv = append(argv, args...)
	return exec.CommandContext(ctx, docker, argv...), nil
}
