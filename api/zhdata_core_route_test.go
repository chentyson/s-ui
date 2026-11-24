package api

// POST api/zhdataCore/update is a two-segment action, which the catch-all
// ":postAction" route cannot match. Gin has to accept the extra registration
// next to the wildcard, and the update has to land behind the same login check
// as every other state-changing call -- a 404 here means the button in the UI
// would silently do nothing.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
)

func apiRoutesEngine(t *testing.T) *gin.Engine {
	t.Helper()
	engine := gin.New()
	store := cookie.NewStore([]byte("test-secret-not-used-anywhere-else"))
	store.Options(BaseSessionOptions(0))
	engine.Use(sessions.Sessions("s-ui", store))
	// apiv2 is only reached by the token actions, so nil keeps this test off the
	// sqlite driver: what is under test is the routing, not the handlers.
	apiv2 := (*APIv2Handler)(nil)
	NewAPIHandler(engine.Group("/api"), apiv2)
	return engine
}

func TestZhdataCoreRoutesAreRegistered(t *testing.T) {
	engine := apiRoutesEngine(t)

	cases := []struct {
		method, path string
	}{
		{http.MethodGet, "/api/zhdataCore"},
		{http.MethodPost, "/api/zhdataCore/update"},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(c.method, c.path, nil)
		// The 401 body only comes back as JSON for an XHR request; either way
		// the status proves the path routed into the api group.
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
		engine.ServeHTTP(rec, req)
		if rec.Code == http.StatusNotFound {
			t.Fatalf("%s %s routed nowhere (404)", c.method, c.path)
		}
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s = %d, want 401 (logged out): %s", c.method, c.path, rec.Code, rec.Body.String())
		}
	}

	// The catch-all still answers, so the extra route did not shadow it.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/zhdataHealth", nil)
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	engine.ServeHTTP(rec, req)
	if rec.Code == http.StatusNotFound {
		t.Fatal("zhdataHealth stopped routing")
	}
}
