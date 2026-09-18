//go:build !cshared

// Command plugin is the development harness.
//
// It boots the real application against an in-memory mock of the CPA host and
// serves the real Management API and admin panel over HTTP, so the plugin can
// be developed and exercised without a CPA instance. The C-ABI shared library
// is built from the same package with the `cshared` build tag.
//
//	go run ./cmd/plugin -data-dir ./.local -listen 127.0.0.1:8787
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/yangshoulai/codex-turn-state-manager/internal/app"
	"github.com/yangshoulai/codex-turn-state-manager/internal/hostapi"
	"github.com/yangshoulai/codex-turn-state-manager/internal/version"
	"github.com/yangshoulai/codex-turn-state-manager/web"
)

func main() {
	var (
		dataDir   = flag.String("data-dir", "./.local", "directory for state.db and backups")
		listen    = flag.String("listen", "127.0.0.1:8787", "address for the harness HTTP server")
		accounts  = flag.Int("accounts", 3, "number of mock Codex accounts to seed")
		upstream  = flag.String("upstream", "", "override the upstream base URL")
		manageKey = flag.String("management-key", "devkey", "expected management key")
		logLevel  = flag.String("log-level", "debug", "debug|info|warn|error")
	)
	flag.Parse()

	level := hostapi.LogLevel(*logLevel)
	logf := func(l hostapi.LogLevel, msg string, fields map[string]any) {
		if !shouldLog(level, l) {
			return
		}
		if len(fields) == 0 {
			log.Printf("[%s] %s", l, msg)
			return
		}
		log.Printf("[%s] %s %v", l, msg, fields)
	}

	host := hostapi.NewMockHost()
	for i := 0; i < *accounts; i++ {
		host.AddAccount(hostapi.Account{
			AuthIndex: authIndexFor(i),
			AuthID:    authIDFor(i),
			Provider:  hostapi.ProviderCodex,
			Label:     labelFor(i),
			Status:    hostapi.AccountStatusAvailable,
			Priority:  10 - i,
		})
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	application, err := app.New(ctx, app.Config{
		DataDir:         *dataDir,
		UpstreamBaseURL: *upstream,
		Host:            host,
		Log:             logf,
	})
	if err != nil {
		log.Fatalf("startup failed: %v", err)
	}
	defer application.Stop()

	application.Start(ctx)
	if _, err := application.SyncAccounts(ctx); err != nil {
		logf(hostapi.LogWarn, "initial sync failed", map[string]any{"error": err.Error()})
	}

	handler := application.Handler(web.Handler())
	srv := &http.Server{
		Addr:              *listen,
		Handler:           requireKey(*manageKey, handler),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("development harness listening on http://%s%s/", *listen, version.ResourceBasePath)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logf(hostapi.LogError, "http server stopped", map[string]any{"error": err.Error()})
			stop()
		}
	}()

	<-ctx.Done()
	log.Println("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}

// requireKey implements the management auth gate. CPA provides this in
// production; the harness needs its own so the panel's auth path is exercised.
//
// Resource routes are intentionally unauthenticated, mirroring CPA: that is
// exactly why they must never serve secrets.
func requireKey(expected string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.URL.Path) >= len("/v0/management/") && r.URL.Path[:len("/v0/management/")] == "/v0/management/" {
			key := r.Header.Get("X-Management-Key")
			if key == "" {
				key = r.URL.Query().Get("key")
			}
			if key != expected {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"invalid management key"}`))
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func shouldLog(threshold, level hostapi.LogLevel) bool {
	order := map[hostapi.LogLevel]int{
		hostapi.LogDebug: 0, hostapi.LogInfo: 1, hostapi.LogWarn: 2, hostapi.LogError: 3,
	}
	want, ok := order[threshold]
	if !ok {
		want = 0
	}
	return order[level] >= want
}

func authIndexFor(i int) string { return "codex-auth-" + itoa(i+1) }
func authIDFor(i int) string    { return "auth-id-" + itoa(i+1) }
func labelFor(i int) string     { return "mock-user-" + itoa(i+1) + "@example.com" }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
