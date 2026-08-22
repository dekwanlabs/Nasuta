package indexing

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/indexing/indexer"
	"github.com/dekwanlabs/nasuta/platform"
)

const codeIndexStateVersion = 1

type codeIndexState struct {
	Version      int                            `json:"version"`
	Repositories map[string]bool                `json:"repositories"`
	Chunks       map[string]codeIndexChunkState `json:"chunks"`
}

type codeIndexChunkState struct {
	ID             string `json:"id"`
	Repo           string `json:"repo"`
	Path           string `json:"path"`
	StartLine      int    `json:"start_line"`
	EndLine        int    `json:"end_line"`
	ContentHash    string `json:"content_hash"`
	ModelIdentity  string `json:"model_identity"`
	Dimension      int    `json:"dimension"`
	PolicyVersion  string `json:"policy_version"`
	ChunkerVersion string `json:"chunker_version"`
}

func newCodeIndexState() codeIndexState {
	return codeIndexState{
		Version:      codeIndexStateVersion,
		Repositories: make(map[string]bool),
		Chunks:       make(map[string]codeIndexChunkState),
	}
}

func loadCodeIndexState(root string) (codeIndexState, error) {
	path := codeIndexStatePath(root)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return newCodeIndexState(), nil
	}
	if err != nil {
		return codeIndexState{}, fmt.Errorf("read code index state %q: %w", path, err)
	}
	var state codeIndexState
	if err := json.Unmarshal(data, &state); err != nil {
		return codeIndexState{}, fmt.Errorf("decode code index state %q: %w", path, err)
	}
	if state.Version != codeIndexStateVersion {
		return codeIndexState{}, fmt.Errorf("code index state %q has version %d, want %d; run a full code rebuild", path, state.Version, codeIndexStateVersion)
	}
	if state.Chunks == nil {
		state.Chunks = make(map[string]codeIndexChunkState)
	}
	if state.Repositories == nil {
		state.Repositories = make(map[string]bool)
	}
	return state, nil
}

func saveCodeIndexState(root string, state codeIndexState) error {
	if state.Version == 0 {
		state.Version = codeIndexStateVersion
	}
	if state.Chunks == nil {
		state.Chunks = make(map[string]codeIndexChunkState)
	}
	if state.Repositories == nil {
		state.Repositories = make(map[string]bool)
	}
	path := codeIndexStatePath(root)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create code index state directory %q: %w", dir, err)
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode code index state: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".code_index_state-*.tmp")
	if err != nil {
		return fmt.Errorf("create code index state temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("restrict code index state permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write code index state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close code index state: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("publish code index state %q: %w", path, err)
	}
	return nil
}

func codeIndexStatePath(root string) string {
	return filepath.Join(root, platform.WorkspaceMetadataDir, "code_index_state.json")
}

func codeIndexChunkStateFor(chunk domain.CodeChunk, modelIdentity string, dimension int) codeIndexChunkState {
	return codeIndexChunkState{
		ID:             codeChunkID(chunk),
		Repo:           chunk.Repo,
		Path:           chunk.Path,
		StartLine:      chunk.StartLine,
		EndLine:        chunk.EndLine,
		ContentHash:    codeChunkContentHash(chunk.Text),
		ModelIdentity:  modelIdentity,
		Dimension:      dimension,
		PolicyVersion:  indexer.CodePolicyVersion,
		ChunkerVersion: indexer.CodeChunkerVersion,
	}
}

func (state codeIndexState) compatible(chunk codeIndexChunkState) bool {
	old, ok := state.Chunks[chunk.ID]
	if !ok {
		return false
	}
	return old.Repo == chunk.Repo &&
		old.Path == chunk.Path &&
		old.StartLine == chunk.StartLine &&
		old.EndLine == chunk.EndLine &&
		old.ContentHash == chunk.ContentHash &&
		old.ModelIdentity == chunk.ModelIdentity &&
		old.Dimension == chunk.Dimension &&
		old.PolicyVersion == chunk.PolicyVersion &&
		old.ChunkerVersion == chunk.ChunkerVersion
}

func (state codeIndexState) repoChunkIDs(repo string) map[string]struct{} {
	ids := make(map[string]struct{})
	for id, chunk := range state.Chunks {
		if chunk.Repo == repo {
			ids[id] = struct{}{}
		}
	}
	return ids
}

func codeChunkContentHash(text string) string {
	sum := sha256.Sum256([]byte(trimText(text)))
	return hex.EncodeToString(sum[:])
}

func codeChunkID(chunk domain.CodeChunk) string {
	return platform.UUIDFromString(
		"code:" + chunk.Path + ":" +
			fmt.Sprintf("%d:%d", chunk.StartLine, chunk.EndLine),
	)
}

func codeEmbeddingIdentity(cfg config.Config, dimension int) string {
	return cfg.EmbeddingProvider + ":" + cfg.EmbeddingModel + ":" + fmt.Sprintf("%d", dimension)
}
