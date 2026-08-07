package indexer

import "testing"

func TestNodeJSRouteCandidatesKeepProvenance(t *testing.T) {
	text := `const createExpress = require("express");
const app = createExpress();
app.get("/health", handler);
cache.get("/not-an-express-route", handler);`

	source := endpointSource{
		language:    "nodejs",
		rel:         "repos/web/orders/routes.js",
		repo:        "web",
		serviceName: "orders",
		modulePath:  "repos/web/orders",
		text:        text,
		syntax:      parseNodeJSSource(text),
	}
	syntax := source.syntax.(nodejsSource)
	if !nodejsImported(syntax, "express") {
		t.Fatal("express require was not recorded")
	}
	if nodejsImported(syntax, "fastify") {
		t.Fatal("unrelated fastify adapter matched express require")
	}

	candidates := scanNodeJSExpress(source)
	if len(candidates) != 1 {
		t.Fatalf("express candidates = %d, want 1: %+v", len(candidates), candidates)
	}
	got := candidates[0]
	if got.ServiceName != "orders" || got.Repo != "web" {
		t.Fatalf("candidate provenance = %q/%q, want orders/web", got.ServiceName, got.Repo)
	}
	if got.Evidence.Path != source.rel || got.Evidence.Line != 3 {
		t.Fatalf("candidate evidence = %+v, want %s:3", got.Evidence, source.rel)
	}
	if paths, ok := literalValues(got.Paths); !ok || len(paths) != 1 || paths[0] != "/health" {
		t.Fatalf("candidate path = %#v/%v, want /health", paths, ok)
	}
}

func TestPythonAliasesReceiversAndDynamicPaths(t *testing.T) {
	text := `from fastapi import FastAPI as App, APIRouter as Router
api = App()
router = Router(prefix="/v1")

@api.get("/health")
def health():
    pass

@router.get(ROUTE_FROM_CONFIG)
def dynamic():
    pass

@cache.get("/not-a-route")
def fake():
    pass
`
	source := endpointSource{
		language:    "python",
		rel:         "repos/ai/catalog/routes.py",
		repo:        "ai",
		serviceName: "catalog",
		modulePath:  "repos/ai/catalog",
		text:        text,
		syntax:      parsePythonSource(text),
	}
	syntax := source.syntax.(pythonSource)
	if got := syntax.imports["App"]; got != "fastapi.FastAPI" {
		t.Fatalf("FastAPI alias import = %q, want fastapi.FastAPI", got)
	}
	if got := syntax.imports["Router"]; got != "fastapi.APIRouter" {
		t.Fatalf("APIRouter alias import = %q, want fastapi.APIRouter", got)
	}

	candidates := scanFastAPI(source)
	if len(candidates) != 2 {
		t.Fatalf("FastAPI candidates = %d, want 2 resolved/unresolved candidates: %+v", len(candidates), candidates)
	}
	records := projectEndpointCandidates(candidates)
	if len(records) != 1 || records[0].Path != "/health" {
		t.Fatalf("projected FastAPI endpoints = %+v, want only /health", records)
	}
}

func TestPythonFrameworkReceiversStaySeparated(t *testing.T) {
	text := `from fastapi import FastAPI
import flask as flasklib
api = FastAPI()
web = flasklib.Flask(__name__)

@api.get("/api")
def api_route():
    pass

@web.get("/web")
def web_route():
    pass
`
	source := endpointSource{
		language:    "python",
		rel:         "repos/mixed/routes.py",
		repo:        "mixed",
		serviceName: "mixed",
		text:        text,
		syntax:      parsePythonSource(text),
	}
	if candidates := scanFastAPI(source); len(candidates) != 1 {
		t.Fatalf("FastAPI candidates = %d, want 1: %+v", len(candidates), candidates)
	}
	if candidates := scanFlask(source); len(candidates) != 1 {
		t.Fatalf("Flask candidates = %d, want 1: %+v", len(candidates), candidates)
	}
}
