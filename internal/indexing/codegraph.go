package indexing

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/dekwanlabs/nasuta/internal/platform/store/codegraph"
	"github.com/dekwanlabs/nasuta/log"
)

func (svc *Service) RebuildGraph(ctx context.Context) error {
	started := time.Now()
	if err := ensureCodegraphConfig(svc.Cfg.WorkspaceRoot); err != nil {
		return fmt.Errorf("configure codegraph: %w", err)
	}
	if err := svc.runCodegraphIndex(ctx); err != nil {
		return fmt.Errorf("rebuild codegraph: %w", err)
	}
	db, err := codegraph.Open(svc.Cfg.WorkspaceRoot)
	if err != nil {
		return fmt.Errorf("validate rebuilt codegraph: %w", err)
	}
	if db == nil {
		return fmt.Errorf("validate rebuilt codegraph: database unavailable")
	}
	if err := db.Close(); err != nil {
		return fmt.Errorf("close rebuilt codegraph: %w", err)
	}
	log.Infof("[codegraph] rebuild completed after %s", time.Since(started).Round(time.Millisecond))
	return nil
}

var codegraphRuntimeExcludes = []string{
	// Tool infrastructure
	".nasuta/",
	".codeloom/",
	".claude/",
	".codex/",
	".docs/",
	"docs/",

	// Dependencies
	"node_modules/",
	"vendor/",
	"Pods/",
	"Carthage/",

	// Build output
	"target/",
	"build/",
	"dist/",
	"out/",
	"bin/",
	"DerivedData/",

	// Framework caches & generated
	".next/",
	".nuxt/",
	".output/",
	".turbo/",
	".angular/",
	".svelte-kit/",
	".parcel-cache/",

	// Python
	"__pycache__/",
	".venv/",
	"venv/",
	".tox/",
	".eggs/",

	// Gradle
	".gradle/",

	// IDE
	".idea/",
	".vscode/",
	".settings/",

	// Test & coverage
	"coverage/",
	".nyc_output/",
	".cache/",
}

func ensureCodegraphConfig(workspaceRoot string) error {
	path := filepath.Join(workspaceRoot, "codegraph.json")
	cfg := make(map[string]json.RawMessage)
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	var excludes []string
	if raw, ok := cfg["exclude"]; ok {
		if err := json.Unmarshal(raw, &excludes); err != nil {
			return fmt.Errorf("parse %s exclude: %w", path, err)
		}
	}
	seen := make(map[string]struct{}, len(excludes))
	for _, pattern := range excludes {
		seen[pattern] = struct{}{}
	}
	for _, pattern := range codegraphRuntimeExcludes {
		if _, ok := seen[pattern]; !ok {
			excludes = append(excludes, pattern)
			seen[pattern] = struct{}{}
		}
	}
	excludeJSON, err := json.Marshal(excludes)
	if err != nil {
		return fmt.Errorf("encode %s exclude: %w", path, err)
	}
	cfg["exclude"] = excludeJSON
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(workspaceRoot, 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func (svc *Service) runCodegraphIndex(ctx context.Context) error {
	if name := svc.Cfg.CodeGraphContainer; name != "" {
		if _, err := exec.LookPath("docker"); err == nil {
			if err := runCodegraphDocker(ctx, name); err == nil {
				return nil
			} else {
				if ctx.Err() != nil {
					return fmt.Errorf("docker codegraph: %w", err)
				}
				log.Warnf("[codegraph] docker exec failed, trying local CLI: %v", err)
			}
		}
	}
	cliPath, err := exec.LookPath("codegraph")
	if err != nil {
		return fmt.Errorf("codegraph unavailable (no docker container %q and no local binary)", svc.Cfg.CodeGraphContainer)
	}
	return runCodegraphAt(ctx, cliPath, svc.Cfg.WorkspaceRoot)
}

const codegraphWorkspace = "/workspace"

func runCodegraphDocker(ctx context.Context, container string) error {
	log.Infof("[codegraph] docker exec %s: init %s", container, codegraphWorkspace)
	if out, err := runWithStream(ctx, "[codegraph]", "docker", "exec", container, "codegraph", "init", codegraphWorkspace); err != nil {
		log.Warnf("[codegraph] init: %v (output: %s)", err, string(out))
	}
	log.Infof("[codegraph] docker exec %s: full index rebuild (this may take a minute or two)", container)
	args := append([]string{"exec", container, "codegraph"}, codegraphIndexArgs(codegraphWorkspace)...)
	out, err := runWithStream(ctx, "[codegraph]", "docker", args...)
	if err != nil {
		return fmt.Errorf("index: %w (output: %s)", err, string(out))
	}
	log.Infof("[codegraph] index complete")
	return nil
}

func runCodegraphAt(ctx context.Context, cliPath, target string) error {
	log.Infof("[codegraph] init %s", target)
	if out, err := runWithStream(ctx, "[codegraph]", cliPath, "init", target); err != nil {
		log.Warnf("[codegraph] init: %v (output: %s)", err, string(out))
	}
	log.Infof("[codegraph] full index rebuild (this may take a minute or two)")
	out, err := runWithStream(ctx, "[codegraph]", cliPath, codegraphIndexArgs(target)...)
	if err != nil {
		return fmt.Errorf("index: %w (output: %s)", err, string(out))
	}
	log.Infof("[codegraph] index complete")
	return nil
}

func codegraphIndexArgs(target string) []string {
	return []string{"index", "--force", "--quiet", target}
}

// runWithStream drains both pipes continuously so verbose CLI output cannot block the child.
func runWithStream(ctx context.Context, prefix string, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	var mu sync.Mutex
	var outBuf strings.Builder
	done := make(chan struct{}, 2)

	for _, reader := range []io.Reader{stdout, stderr} {
		go func(reader io.Reader) {
			defer func() { done <- struct{}{} }()
			err := drainStream(reader, func(chunk []byte) {
				mu.Lock()
				outBuf.Write(chunk)
				mu.Unlock()
				log.Infof("%s %s", prefix, strings.TrimRight(string(chunk), "\r\n"))
			})
			if err != nil {
				log.Warnf("%s output reader: %v", prefix, err)
			}
		}(reader)
	}

	<-done
	<-done
	err = cmd.Wait()
	return []byte(outBuf.String()), err
}

// drainStream emits bounded chunks, including fragments of lines larger than the buffer.
func drainStream(reader io.Reader, consume func([]byte)) error {
	buffered := bufio.NewReaderSize(reader, 64*1024)
	for {
		chunk, err := buffered.ReadSlice('\n')
		if len(chunk) > 0 {
			consume(chunk)
		}
		switch {
		case err == nil, errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			return nil
		default:
			return err
		}
	}
}
