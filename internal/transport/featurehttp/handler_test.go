package featurehttp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dekwanlabs/nasuta/internal/auth"
	"github.com/dekwanlabs/nasuta/internal/featuredelivery"
)

type downloadStore struct {
	featuredelivery.Store
	feature featuredelivery.FeatureRequest
	run     featuredelivery.ImplementationRun
}

type generationAuditStore struct {
	featuredelivery.Store
	feature featuredelivery.FeatureRequest
	run     featuredelivery.GenerationRun
}

func (store *generationAuditStore) GetFeature(_ context.Context, id string) (*featuredelivery.FeatureRequest, error) {
	if id != store.feature.ID {
		return nil, featuredelivery.ErrNotFound
	}
	feature := store.feature
	return &feature, nil
}

func (store *generationAuditStore) GetGenerationRun(_ context.Context, id string) (*featuredelivery.GenerationRun, error) {
	if id != store.run.ID {
		return nil, featuredelivery.ErrNotFound
	}
	run := store.run
	return &run, nil
}

func (store *generationAuditStore) ListGenerationRuns(_ context.Context, requestID string, _ featuredelivery.GenerationCursor, _ int) ([]featuredelivery.GenerationRun, error) {
	if requestID != store.feature.ID {
		return nil, featuredelivery.ErrNotFound
	}
	return []featuredelivery.GenerationRun{store.run}, nil
}

func (store *downloadStore) GetFeature(_ context.Context, id string) (*featuredelivery.FeatureRequest, error) {
	if id != store.feature.ID {
		return nil, featuredelivery.ErrNotFound
	}
	feature := store.feature
	return &feature, nil
}

func (store *downloadStore) GetImplementation(_ context.Context, id string) (*featuredelivery.ImplementationRun, error) {
	if id != store.run.ID {
		return nil, featuredelivery.ErrNotFound
	}
	run := store.run
	return &run, nil
}

func TestRegisterRoutesIncludesFeatureDeliverySurface(t *testing.T) {
	mux := http.NewServeMux()
	New(nil).RegisterRoutes(func(pattern string, handler http.HandlerFunc) {
		mux.HandleFunc(pattern, handler)
	})
	for _, target := range []struct {
		method string
		path   string
	}{
		{"POST", "/api/features"},
		{"GET", "/api/features/feat-1"},
		{"POST", "/api/features/feat-1/artifacts/system_design/generate"},
		{"GET", "/api/features/feat-1/generations"},
		{"GET", "/api/feature-generations/gen-1"},
		{"POST", "/api/features/feat-1/implementations"},
		{"GET", "/api/feature-implementations/run-1/events"},
		{"GET", "/api/feature-implementations/run-1/patch"},
		{"GET", "/api/feature-implementations/run-1/validations/1/output"},
	} {
		request, err := http.NewRequest(target.method, target.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, pattern := mux.Handler(request)
		if pattern == "" {
			t.Fatalf("route not registered: %s %s", target.method, target.path)
		}
	}
}

func TestAdministrativeMutationsRejectRegularUsers(t *testing.T) {
	mux := http.NewServeMux()
	New(nil).RegisterRoutes(func(pattern string, handler http.HandlerFunc) { mux.HandleFunc(pattern, handler) })
	paths := []string{
		"/api/features/feat-1/artifacts/artifact-1/review",
		"/api/features/feat-1/implementations",
		"/api/feature-implementations/run-1/cancel",
		"/api/feature-implementations/run-1/review",
	}
	for _, path := range paths {
		request := httptest.NewRequest(http.MethodPost, path, nil)
		request = request.WithContext(auth.WithUser(request.Context(), &auth.User{ID: 7}))
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, request)
		if response.Code != http.StatusForbidden {
			t.Fatalf("POST %s status=%d, want %d", path, response.Code, http.StatusForbidden)
		}
	}
}

func TestListArtifactsRequiresMatchingKind(t *testing.T) {
	handler := New(nil)
	cursor := encodeCursor(artifactCursorPayload{Kind: featuredelivery.KindSystemDesign, Version: 2})
	for _, target := range []struct {
		name string
		url  string
	}{
		{name: "missing kind", url: "/api/features/feat-1/artifacts"},
		{name: "cursor kind mismatch", url: "/api/features/feat-1/artifacts?kind=requirement_analysis&cursor=" + cursor},
	} {
		t.Run(target.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, target.url, nil)
			request = request.WithContext(auth.WithUser(request.Context(), &auth.User{ID: 7}))
			response := httptest.NewRecorder()

			handler.ListArtifacts(response, request)

			if response.Code != http.StatusBadRequest {
				t.Fatalf("status=%d, want %d", response.Code, http.StatusBadRequest)
			}
		})
	}
}

func TestDownloadValidationOutput(t *testing.T) {
	content := []byte("go test ./...\nok\n")
	handler, _ := validationDownloadHandler(t, content, nil)
	request := httptest.NewRequest(http.MethodGet, "/api/feature-implementations/run-1/validations/1/output", nil)
	request = request.WithContext(auth.WithUser(request.Context(), &auth.User{ID: 7}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK || response.Body.String() != string(content) {
		t.Fatalf("download status=%d body=%q", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Content-Disposition"); got != `inline; filename="run-1-validation-01.log"` {
		t.Fatalf("Content-Disposition = %q", got)
	}
	if got := response.Header().Get("Content-Length"); got != "17" {
		t.Fatalf("Content-Length = %q", got)
	}
}

func TestDownloadPatch(t *testing.T) {
	content := []byte("diff --git a/file.go b/file.go\n")
	handler := patchDownloadHandler(t, content, nil)
	request := httptest.NewRequest(http.MethodGet, "/api/feature-implementations/run-1/patch", nil)
	request = request.WithContext(auth.WithUser(request.Context(), &auth.User{ID: 7}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK || response.Body.String() != string(content) {
		t.Fatalf("download status=%d body=%q", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Content-Disposition"); got != `attachment; filename="run-1.patch"` {
		t.Fatalf("Content-Disposition = %q", got)
	}
	if got := response.Header().Get("Content-Length"); got != "31" {
		t.Fatalf("Content-Length = %q", got)
	}
}

func TestDownloadPatchEnforcesAuthenticationAndOwnership(t *testing.T) {
	handler := patchDownloadHandler(t, []byte("patch"), nil)
	for _, test := range []struct {
		name string
		user *auth.User
		want int
	}{
		{name: "unauthenticated", want: http.StatusUnauthorized},
		{name: "other user hidden", user: &auth.User{ID: 8}, want: http.StatusNotFound},
		{name: "administrator", user: &auth.User{ID: 8, IsAdmin: true}, want: http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/api/feature-implementations/run-1/patch", nil)
			if test.user != nil {
				request = request.WithContext(auth.WithUser(request.Context(), test.user))
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d", response.Code, test.want)
			}
		})
	}
}

func TestDownloadPatchVerifiesMetadataAndHash(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*featuredelivery.ChangeSet)
	}{
		{name: "size mismatch", mutate: func(change *featuredelivery.ChangeSet) { change.PatchBytes++ }},
		{name: "hash mismatch", mutate: func(change *featuredelivery.ChangeSet) { change.PatchSHA256 = strings.Repeat("0", 64) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := patchDownloadHandler(t, []byte("patch"), test.mutate)
			request := httptest.NewRequest(http.MethodGet, "/api/feature-implementations/run-1/patch", nil)
			request = request.WithContext(auth.WithUser(request.Context(), &auth.User{ID: 7}))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want %d", response.Code, http.StatusInternalServerError)
			}
		})
	}
}

func TestGenerationAuditRoutesEnforceAuthenticationAndOwnership(t *testing.T) {
	store := &generationAuditStore{
		feature: featuredelivery.FeatureRequest{ID: "feat-1", CreatedBy: 7},
		run:     featuredelivery.GenerationRun{ID: "gen-1", RequestID: "feat-1"},
	}
	mux := http.NewServeMux()
	New(featuredelivery.NewService(store, nil, time.Minute)).RegisterRoutes(func(pattern string, handler http.HandlerFunc) {
		mux.HandleFunc(pattern, handler)
	})
	tests := []struct {
		name string
		path string
		user *auth.User
		want int
	}{
		{name: "detail unauthenticated", path: "/api/feature-generations/gen-1", want: http.StatusUnauthorized},
		{name: "detail owner", path: "/api/feature-generations/gen-1", user: &auth.User{ID: 7}, want: http.StatusOK},
		{name: "detail other user hidden", path: "/api/feature-generations/gen-1", user: &auth.User{ID: 8}, want: http.StatusNotFound},
		{name: "detail administrator", path: "/api/feature-generations/gen-1", user: &auth.User{ID: 8, IsAdmin: true}, want: http.StatusOK},
		{name: "list owner", path: "/api/features/feat-1/generations?limit=10", user: &auth.User{ID: 7}, want: http.StatusOK},
		{name: "list other user hidden", path: "/api/features/feat-1/generations?limit=10", user: &auth.User{ID: 8}, want: http.StatusNotFound},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			if test.user != nil {
				request = request.WithContext(auth.WithUser(request.Context(), test.user))
			}
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, test.want, response.Body.String())
			}
		})
	}
}

func TestDownloadValidationOutputRejectsUnauthenticatedAndInvalidSequence(t *testing.T) {
	mux := http.NewServeMux()
	New(nil).RegisterRoutes(func(pattern string, handler http.HandlerFunc) { mux.HandleFunc(pattern, handler) })
	tests := []struct {
		name string
		path string
		user *auth.User
		want int
	}{
		{name: "unauthenticated", path: "/api/feature-implementations/run-1/validations/1/output", want: http.StatusUnauthorized},
		{name: "invalid sequence", path: "/api/feature-implementations/run-1/validations/not-a-number/output", user: &auth.User{ID: 7}, want: http.StatusBadRequest},
		{name: "zero sequence", path: "/api/feature-implementations/run-1/validations/0/output", user: &auth.User{ID: 7}, want: http.StatusBadRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			if test.user != nil {
				request = request.WithContext(auth.WithUser(request.Context(), test.user))
			}
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d", response.Code, test.want)
			}
		})
	}
}

func TestDownloadValidationOutputVerifiesMetadataAndHash(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*featuredelivery.ValidationResult)
	}{
		{name: "size mismatch", mutate: func(result *featuredelivery.ValidationResult) { result.OutputBytes++ }},
		{name: "hash mismatch", mutate: func(result *featuredelivery.ValidationResult) { result.OutputSHA256 = strings.Repeat("0", 64) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler, _ := validationDownloadHandler(t, []byte("validation output"), test.mutate)
			request := httptest.NewRequest(http.MethodGet, "/api/feature-implementations/run-1/validations/1/output", nil)
			request = request.WithContext(auth.WithUser(request.Context(), &auth.User{ID: 7}))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want %d", response.Code, http.StatusInternalServerError)
			}
		})
	}
}

func validationDownloadHandler(t *testing.T, content []byte, mutate func(*featuredelivery.ValidationResult)) (http.Handler, *featuredelivery.ValidationResult) {
	t.Helper()
	workspaceRoot := t.TempDir()
	codingRoot := t.TempDir()
	store := &downloadStore{feature: featuredelivery.FeatureRequest{ID: "feat-1", CreatedBy: 7}}
	workspaces, err := featuredelivery.NewWorkspaceManager(store, codingRoot)
	if err != nil {
		t.Fatal(err)
	}
	git, err := featuredelivery.NewGitManager(workspaceRoot, codingRoot, workspaces)
	if err != nil {
		t.Fatal(err)
	}
	artifactDir := filepath.Join(codingRoot, "artifacts", "run-1")
	if err := os.MkdirAll(artifactDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artifactDir, "validation-01.log"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	validation := featuredelivery.ValidationResult{
		Sequence: 1, OutputRelPath: "run-1/validation-01.log",
		OutputSHA256: hex.EncodeToString(sum[:]), OutputBytes: int64(len(content)),
	}
	if mutate != nil {
		mutate(&validation)
	}
	store.run = featuredelivery.ImplementationRun{
		ID: "run-1", RequestID: "feat-1",
		ChangeSet: &featuredelivery.ChangeSet{ValidationResults: []featuredelivery.ValidationResult{validation}},
	}
	service := featuredelivery.NewService(store, nil, time.Minute)
	service.SetImplementationManager(featuredelivery.NewImplementationManager(
		store, workspaces, git, nil, featuredelivery.ImplementationConfig{},
	))
	mux := http.NewServeMux()
	New(service).RegisterRoutes(func(pattern string, handler http.HandlerFunc) { mux.HandleFunc(pattern, handler) })
	return mux, &store.run.ChangeSet.ValidationResults[0]
}

func patchDownloadHandler(t *testing.T, content []byte, mutate func(*featuredelivery.ChangeSet)) http.Handler {
	t.Helper()
	workspaceRoot := t.TempDir()
	codingRoot := t.TempDir()
	store := &downloadStore{feature: featuredelivery.FeatureRequest{ID: "feat-1", CreatedBy: 7}}
	workspaces, err := featuredelivery.NewWorkspaceManager(store, codingRoot)
	if err != nil {
		t.Fatal(err)
	}
	git, err := featuredelivery.NewGitManager(workspaceRoot, codingRoot, workspaces)
	if err != nil {
		t.Fatal(err)
	}
	artifactDir := filepath.Join(codingRoot, "artifacts", "run-1")
	if err := os.MkdirAll(artifactDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artifactDir, "changes.patch"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	change := &featuredelivery.ChangeSet{
		RunID: "run-1", PatchRelPath: "run-1/changes.patch",
		PatchSHA256: hex.EncodeToString(sum[:]), PatchBytes: int64(len(content)),
	}
	if mutate != nil {
		mutate(change)
	}
	store.run = featuredelivery.ImplementationRun{
		ID: "run-1", RequestID: "feat-1", ChangeSet: change,
	}
	service := featuredelivery.NewService(store, nil, time.Minute)
	service.SetImplementationManager(featuredelivery.NewImplementationManager(
		store, workspaces, git, nil, featuredelivery.ImplementationConfig{},
	))
	mux := http.NewServeMux()
	New(service).RegisterRoutes(func(pattern string, handler http.HandlerFunc) { mux.HandleFunc(pattern, handler) })
	return mux
}
